package progress

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
	SchemaDeliveryObligation = "loopcoder.progress_delivery_obligation.v1"
	SchemaDeliveryAttempt    = "loopcoder.progress_delivery_attempt.v1"
	SchemaDeliveryResult     = "loopcoder.progress_delivery_attempt_result.v1"
	SchemaDeliveryAck        = "loopcoder.progress_delivery_acknowledgment.v1"
	SchemaDeliveryCursor     = "loopcoder.progress_delivery_replay_cursor.v1"

	DeliveryPending                 = "pending"
	DeliveryAttempting              = "attempting"
	DeliveryNeedsReconciliation     = "needs-reconciliation"
	DeliveryDeliveredUnacknowledged = "delivered-unacknowledged"
	DeliveryAcknowledged            = "acknowledged"
	DeliveryUnsupported             = "unsupported"
	DeliveryRetryableFailure        = "retryable-failure"
	DeliveryTerminalFailure         = "terminal-failure"
	DeliveryExpired                 = "expired"
	DeliverySuperseded              = "superseded"

	DeliveryAckPolicyRequired = "required-ack"
	DeliveryAckPolicyNone     = "no-ack"
)

type DeliveryEvidence struct {
	EvidenceKind      string `json:"evidence_kind"`
	EvidenceRef       string `json:"evidence_ref"`
	Summary           string `json:"summary"`
	Confidence        string `json:"confidence"`
	TransportContract string `json:"transport_contract"`
}

type DeliveryObligation struct {
	SchemaVersion            string           `json:"schema_version"`
	RecordVersion            int              `json:"record_version"`
	ObligationID             string           `json:"obligation_id"`
	ProjectID                string           `json:"project_id"`
	DeliveryRunID            string           `json:"delivery_run_id"`
	ProgressReceiptID        string           `json:"progress_receipt_id"`
	SemanticIdentity         string           `json:"semantic_identity"`
	OriginKind               string           `json:"origin_kind"`
	OriginID                 string           `json:"origin_id"`
	SinkKind                 string           `json:"sink_kind"`
	SinkID                   string           `json:"sink_id"`
	TransportContract        string           `json:"transport_contract"`
	Status                   string           `json:"status"`
	ClaimOwner               string           `json:"claim_owner,omitempty"`
	ClaimGeneration          int64            `json:"claim_generation"`
	ClaimedAt                string           `json:"claimed_at,omitempty"`
	ClaimExpiresAt           string           `json:"claim_expires_at,omitempty"`
	AttemptCount             int              `json:"attempt_count"`
	MaxAttempts              int              `json:"max_attempts"`
	NextAttemptAt            string           `json:"next_attempt_at,omitempty"`
	AckPolicy                string           `json:"ack_policy"`
	RequiredAck              bool             `json:"required_ack"`
	ExpiresAt                string           `json:"expires_at,omitempty"`
	SupersededByObligationID string           `json:"superseded_by_obligation_id,omitempty"`
	LastErrorCode            string           `json:"last_error_code,omitempty"`
	CreatedAt                string           `json:"created_at"`
	UpdatedAt                string           `json:"updated_at"`
	LatestEvidence           DeliveryEvidence `json:"latest_evidence,omitempty"`
}

type DeliveryBinding struct {
	OriginKind        string
	OriginID          string
	SinkKind          string
	SinkID            string
	TransportContract string
	AckPolicy         string
	MaxAttempts       int
}

type DeliveryObligationFunc func(ProgressReceipt) (DeliveryObligation, bool)

type PersistReceiptWithObligationResult struct {
	Receipt    WriteResult
	Obligation DeliveryObligation
	Inserted   bool
}

type ClaimRequest struct {
	ObligationID   string
	ClaimOwner     string
	LeaseExpiresAt string
}

type ClaimResult struct {
	ObligationID    string `json:"obligation_id"`
	ClaimOwner      string `json:"claim_owner"`
	ClaimGeneration int64  `json:"claim_generation"`
	LeaseExpiresAt  string `json:"lease_expires_at"`
	Status          string `json:"status"`
}

type AttemptResultRequest struct {
	ObligationID           string
	AttemptRecordID        string
	AttemptOrdinal         int
	ClaimOwner             string
	Generation             int64
	ProviderIdempotencyKey string
	ResultStatus           string
	Evidence               DeliveryEvidence
	ErrorCode              string
	NextAttemptAt          string
	StartedAt              string
	CompletedAt            string
}

type AcknowledgmentRequest struct {
	ObligationID           string
	AttemptRecordID        string
	AttemptOrdinal         int
	ResultRecordID         string
	ClaimOwner             string
	Generation             int64
	ProviderIdempotencyKey string
	Evidence               DeliveryEvidence
	AckedAt                string
}

type BeginAttemptRequest struct {
	ObligationID string
	ClaimOwner   string
	Generation   int64
	PreparedAt   string
}

type CursorAdvanceRequest struct {
	ObligationID string
	ClaimOwner   string
	Generation   int64
	OriginKind   string
	OriginID     string
	CursorValue  string
	AdvancedAt   string
}

type DeliveryObligationFilter struct {
	ProjectID     string
	DeliveryRunID string
	Status        string
	SinkKind      string
	SinkID        string
	DueAtOrBefore string
	Limit         int
}

type DeliveryAttemptFilter struct {
	ProjectID     string
	DeliveryRunID string
	ObligationID  string
	Limit         int
}

type DeliveryAckFilter struct {
	ProjectID     string
	DeliveryRunID string
	ObligationID  string
	Limit         int
}

type DeliveryCursorFilter struct {
	ProjectID     string
	DeliveryRunID string
	OriginKind    string
	OriginID      string
	Limit         int
}

type DeliveryAttempt struct {
	SchemaVersion          string           `json:"schema_version"`
	RecordVersion          int              `json:"record_version"`
	AttemptRecordID        string           `json:"attempt_record_id"`
	ObligationID           string           `json:"obligation_id"`
	AttemptOrdinal         int              `json:"attempt_ordinal"`
	ClaimOwner             string           `json:"claim_owner"`
	ClaimGeneration        int64            `json:"claim_generation"`
	TransportContract      string           `json:"transport_contract"`
	ProviderIdempotencyKey string           `json:"provider_idempotency_key"`
	ResultRecordID         string           `json:"result_record_id,omitempty"`
	ResultStatus           string           `json:"result_status,omitempty"`
	NextAttemptAt          string           `json:"next_attempt_at,omitempty"`
	Evidence               DeliveryEvidence `json:"evidence,omitempty"`
	ErrorCode              string           `json:"error_code,omitempty"`
	PreparedAt             string           `json:"prepared_at"`
	StartedAt              string           `json:"started_at,omitempty"`
	CompletedAt            string           `json:"completed_at,omitempty"`
}

type DeliveryAttemptResult struct {
	SchemaVersion          string           `json:"schema_version"`
	RecordVersion          int              `json:"record_version"`
	ResultRecordID         string           `json:"result_record_id"`
	AttemptRecordID        string           `json:"attempt_record_id"`
	ObligationID           string           `json:"obligation_id"`
	AttemptOrdinal         int              `json:"attempt_ordinal"`
	ClaimOwner             string           `json:"claim_owner"`
	ClaimGeneration        int64            `json:"claim_generation"`
	TransportContract      string           `json:"transport_contract"`
	ProviderIdempotencyKey string           `json:"provider_idempotency_key"`
	ResultStatus           string           `json:"result_status"`
	NextAttemptAt          string           `json:"next_attempt_at,omitempty"`
	Evidence               DeliveryEvidence `json:"evidence"`
	ErrorCode              string           `json:"error_code,omitempty"`
	StartedAt              string           `json:"started_at"`
	CompletedAt            string           `json:"completed_at"`
}

type DeliveryAcknowledgment struct {
	SchemaVersion          string           `json:"schema_version"`
	RecordVersion          int              `json:"record_version"`
	AcknowledgmentID       string           `json:"acknowledgment_id"`
	ObligationID           string           `json:"obligation_id"`
	SemanticIdentity       string           `json:"semantic_identity"`
	AttemptRecordID        string           `json:"attempt_record_id"`
	AttemptOrdinal         int              `json:"attempt_ordinal"`
	ResultRecordID         string           `json:"result_record_id"`
	ClaimOwner             string           `json:"claim_owner"`
	ClaimGeneration        int64            `json:"claim_generation"`
	TransportContract      string           `json:"transport_contract"`
	ProviderIdempotencyKey string           `json:"provider_idempotency_key"`
	Evidence               DeliveryEvidence `json:"evidence"`
	AcknowledgedAt         string           `json:"acknowledged_at"`
}

type DeliveryReplayCursor struct {
	SchemaVersion   string `json:"schema_version"`
	RecordVersion   int    `json:"record_version"`
	CursorID        string `json:"cursor_id"`
	ObligationID    string `json:"obligation_id"`
	ClaimOwner      string `json:"claim_owner"`
	ClaimGeneration int64  `json:"claim_generation"`
	OriginKind      string `json:"origin_kind"`
	OriginID        string `json:"origin_id"`
	CursorValue     string `json:"cursor_value"`
	AdvancedAt      string `json:"advanced_at"`
}

type deliveryObligationReplayCandidate struct {
	Current DeliveryObligation
	Initial DeliveryObligation
}

func PersistReceiptWithObligation(ctx context.Context, store storage.Store, receipt ProgressReceipt, obligation DeliveryObligation) (PersistReceiptWithObligationResult, error) {
	return persistReceiptWithObligation(ctx, store, receipt, false, func(ProgressReceipt) (DeliveryObligation, bool) {
		return obligation, true
	})
}

func PersistReceiptNextSequenceWithObligation(ctx context.Context, store storage.Store, receipt ProgressReceipt, obligation DeliveryObligation) (PersistReceiptWithObligationResult, error) {
	return persistReceiptWithObligation(ctx, store, receipt, true, func(ProgressReceipt) (DeliveryObligation, bool) {
		return obligation, true
	})
}

func PersistReceiptNextSequenceWithObligationFunc(ctx context.Context, store storage.Store, receipt ProgressReceipt, obligation DeliveryObligationFunc) (PersistReceiptWithObligationResult, error) {
	return persistReceiptWithObligation(ctx, store, receipt, true, obligation)
}

func DeliveryObligationForBinding(binding DeliveryBinding) DeliveryObligationFunc {
	return func(receipt ProgressReceipt) (DeliveryObligation, bool) {
		return DeliveryObligation{
			ProjectID:         receipt.ProjectID,
			DeliveryRunID:     receipt.DeliveryRunID,
			ProgressReceiptID: receipt.ProgressReceiptID,
			OriginKind:        binding.OriginKind,
			OriginID:          binding.OriginID,
			SinkKind:          binding.SinkKind,
			SinkID:            binding.SinkID,
			TransportContract: binding.TransportContract,
			AckPolicy:         binding.AckPolicy,
			MaxAttempts:       binding.MaxAttempts,
		}, true
	}
}

func persistReceiptWithObligation(ctx context.Context, store storage.Store, receipt ProgressReceipt, nextSequence bool, obligation DeliveryObligationFunc) (PersistReceiptWithObligationResult, error) {
	if store == nil {
		return PersistReceiptWithObligationResult{}, typed(ErrInvalidRecordCode, "store is required")
	}
	if obligation == nil {
		return PersistReceiptWithObligationResult{}, typed(ErrInvalidRecordCode, "delivery obligation function is required")
	}
	now := store.Now()
	normalizedReceipt, err := NormalizeReceipt(receipt, now)
	if err != nil {
		return PersistReceiptWithObligationResult{}, err
	}
	var result PersistReceiptWithObligationResult
	err = store.WithWriteTx(ctx, func(tx storage.Tx) error {
		receiptResult, err := persistNormalizedReceiptTx(ctx, tx, normalizedReceipt, now, nextSequence)
		if err != nil {
			return err
		}
		obligationRecord, ok := obligation(receiptResult.Receipt)
		if !ok {
			result = PersistReceiptWithObligationResult{Receipt: receiptResult}
			return nil
		}
		normalizedObligation, err := normalizeDeliveryObligation(obligationRecord, receiptResult.Receipt, now)
		if err != nil {
			return err
		}
		stored, inserted, err := persistDeliveryObligationTx(ctx, tx, normalizedObligation)
		if err != nil {
			return err
		}
		result = PersistReceiptWithObligationResult{Receipt: receiptResult, Obligation: stored, Inserted: inserted}
		return nil
	})
	return result, err
}

func ClaimDeliveryObligation(ctx context.Context, store storage.Store, req ClaimRequest) (ClaimResult, error) {
	if store == nil {
		return ClaimResult{}, typed(ErrInvalidRecordCode, "store is required")
	}
	now := store.Now()
	obligationID := sanitizeID(req.ObligationID)
	owner := sanitizeID(req.ClaimOwner)
	leaseExpiresAt := normalizeTimestampOr(req.LeaseExpiresAt, "")
	if obligationID == "" || owner == "" || leaseExpiresAt == "" {
		return ClaimResult{}, typed(ErrInvalidRecordCode, "obligation_id, claim_owner, and lease_expires_at are required")
	}
	if !timestampAfter(leaseExpiresAt, now) {
		return ClaimResult{}, typed(ErrInvalidRecordCode, "lease_expires_at must be after now")
	}
	var result ClaimResult
	var committedConflict error
	err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		obligation, err := loadDeliveryObligationTx(ctx, tx, obligationID)
		if err != nil {
			return err
		}
		if terminalDeliveryStatus(obligation.Status) {
			return typed(ErrClaimConflictCode, "obligation %s is terminal in state %s", obligationID, obligation.Status)
		}
		if obligation.Status == DeliveryDeliveredUnacknowledged {
			return typed(ErrClaimConflictCode, "obligation %s is awaiting acknowledgment", obligationID)
		}
		if obligation.Status == DeliveryRetryableFailure && obligation.NextAttemptAt != "" && timestampAfter(obligation.NextAttemptAt, now) {
			return typed(ErrClaimConflictCode, "obligation %s retry is scheduled for %s", obligationID, obligation.NextAttemptAt)
		}
		if obligation.Status == DeliveryNeedsReconciliation {
			return typed(ErrClaimConflictCode, "obligation %s needs delivery reconciliation", obligationID)
		}
		if obligation.Status == DeliveryAttempting && (obligation.ClaimExpiresAt == "" || !timestampAfter(obligation.ClaimExpiresAt, now)) {
			attempt, found, err := loadUnresolvedDeliveryAttemptTx(ctx, tx, obligation.ObligationID)
			if err != nil {
				return err
			}
			if found {
				updatedAt := canonicalTimestamp(now)
				res, err := tx.Exec(ctx, `UPDATE progress_delivery_obligations SET status = ?, updated_at = ?
					WHERE obligation_id = ? AND status = ? AND claim_owner = ? AND claim_generation = ?`,
					DeliveryNeedsReconciliation, updatedAt, obligation.ObligationID, DeliveryAttempting, attempt.ClaimOwner, attempt.ClaimGeneration)
				if err != nil {
					return fmt.Errorf("mark delivery obligation needs reconciliation: %w", err)
				}
				rows, err := res.RowsAffected()
				if err != nil {
					return fmt.Errorf("mark delivery obligation needs reconciliation rows affected: %w", err)
				}
				if rows != 1 {
					return typed(ErrStaleClaimCode, "obligation %s reconciliation lost active claim fence", obligationID)
				}
				committedConflict = typed(ErrClaimConflictCode, "obligation %s has prepared attempt %s with unknown result", obligationID, attempt.AttemptRecordID)
				return nil
			}
		}
		if obligation.Status == DeliveryAttempting && obligation.ClaimExpiresAt != "" && timestampAfter(obligation.ClaimExpiresAt, now) &&
			(obligation.ClaimOwner != owner || obligation.ClaimGeneration > 0) {
			return typed(ErrClaimConflictCode, "obligation %s is leased by a current claimant", obligationID)
		}
		nextGeneration := obligation.ClaimGeneration + 1
		updatedAt := canonicalTimestamp(now)
		res, err := tx.Exec(ctx, `UPDATE progress_delivery_obligations
			SET status = ?, claim_owner = ?, claim_generation = ?, claimed_at = ?, claim_expires_at = ?, updated_at = ?
			WHERE obligation_id = ? AND claim_generation = ?`,
			DeliveryAttempting, owner, nextGeneration, updatedAt, leaseExpiresAt, updatedAt, obligationID, obligation.ClaimGeneration)
		if err != nil {
			return fmt.Errorf("claim delivery obligation: %w", err)
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("claim delivery obligation rows affected: %w", err)
		}
		if rows != 1 {
			return typed(ErrStaleClaimCode, "obligation %s claim generation changed", obligationID)
		}
		result = ClaimResult{ObligationID: obligationID, ClaimOwner: owner, ClaimGeneration: nextGeneration, LeaseExpiresAt: leaseExpiresAt, Status: DeliveryAttempting}
		return nil
	})
	if err != nil {
		return result, err
	}
	if committedConflict != nil {
		return result, committedConflict
	}
	return result, nil
}

func BeginDeliveryAttempt(ctx context.Context, store storage.Store, req BeginAttemptRequest) (DeliveryAttempt, error) {
	if store == nil {
		return DeliveryAttempt{}, typed(ErrInvalidRecordCode, "store is required")
	}
	now := store.Now()
	req.ObligationID = sanitizeID(req.ObligationID)
	req.ClaimOwner = sanitizeID(req.ClaimOwner)
	req.PreparedAt = normalizeTimestampOr(req.PreparedAt, canonicalTimestamp(now))
	if req.ObligationID == "" || req.ClaimOwner == "" || req.Generation <= 0 {
		return DeliveryAttempt{}, typed(ErrInvalidRecordCode, "obligation_id, claim_owner, and generation are required")
	}
	var prepared DeliveryAttempt
	err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		obligation, err := loadDeliveryObligationTx(ctx, tx, req.ObligationID)
		if err != nil {
			return err
		}
		if existing, found, err := loadDeliveryAttemptForGenerationTx(ctx, tx, req.ObligationID, req.Generation); err != nil {
			return err
		} else if found {
			if existing.ClaimOwner != req.ClaimOwner || existing.ClaimGeneration != req.Generation {
				return typed(ErrDuplicateReplayCode, "prepared delivery attempt generation %d conflicts with claimant", req.Generation)
			}
			prepared = existing
			return nil
		}
		if obligation.Status != DeliveryAttempting {
			return typed(ErrStaleClaimCode, "obligation %s is not actively attempting", obligation.ObligationID)
		}
		if err := requireCurrentClaim(obligation, req.ClaimOwner, req.Generation, now); err != nil {
			return err
		}
		if obligation.AttemptCount >= obligation.MaxAttempts {
			return typed(ErrClaimConflictCode, "obligation %s exhausted %d delivery attempts", obligation.ObligationID, obligation.MaxAttempts)
		}
		nextAttempt := obligation.AttemptCount + 1
		attemptID := prefixedDigest("pdelatt", map[string]any{
			"schema_version": SchemaDeliveryAttempt,
			"obligation_id":  req.ObligationID,
			"attempt":        nextAttempt,
		})
		idempotencyKey := prefixedDigest("pdelidem", map[string]any{
			"schema_version":      "loopcoder.progress_delivery_provider_idempotency.v1",
			"obligation_identity": obligation.SemanticIdentity,
			"transport_contract":  obligation.TransportContract,
			"attempt":             nextAttempt,
		})
		payload := map[string]any{
			"schema_version":           SchemaDeliveryAttempt,
			"record_version":           1,
			"attempt_record_id":        attemptID,
			"obligation_id":            req.ObligationID,
			"attempt_ordinal":          nextAttempt,
			"claim_owner":              req.ClaimOwner,
			"claim_generation":         req.Generation,
			"transport_contract":       obligation.TransportContract,
			"provider_idempotency_key": idempotencyKey,
			"prepared_at":              req.PreparedAt,
		}
		payloadJSON, err := canonicalJSON(payload)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO progress_delivery_attempts(
				attempt_record_id, schema_version, record_version, project_id, delivery_run_id, obligation_id,
				attempt_ordinal, claim_owner, claim_generation, transport_contract, provider_idempotency_key,
				prepared_at, payload_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			attemptID, SchemaDeliveryAttempt, 1, obligation.ProjectID, obligation.DeliveryRunID, obligation.ObligationID,
			nextAttempt, req.ClaimOwner, req.Generation, obligation.TransportContract, idempotencyKey, req.PreparedAt, string(payloadJSON)); err != nil {
			return fmt.Errorf("prepare delivery attempt: %w", err)
		}
		updatedAt := canonicalTimestamp(now)
		update, err := tx.Exec(ctx, `UPDATE progress_delivery_obligations
			SET attempt_count = ?, updated_at = ?
			WHERE obligation_id = ? AND status = ? AND claim_owner = ? AND claim_generation = ? AND attempt_count = ?`,
			nextAttempt, updatedAt, req.ObligationID, DeliveryAttempting, req.ClaimOwner, req.Generation, obligation.AttemptCount)
		if err != nil {
			return fmt.Errorf("update delivery obligation prepared attempt: %w", err)
		}
		rows, err := update.RowsAffected()
		if err != nil {
			return fmt.Errorf("update delivery obligation prepared attempt rows affected: %w", err)
		}
		if rows != 1 {
			return typed(ErrStaleClaimCode, "obligation %s prepared attempt lost active claim fence", obligation.ObligationID)
		}
		prepared, err = loadDeliveryAttemptTx(ctx, tx, attemptID)
		return err
	})
	return prepared, err
}

func RecordDeliveryAttemptResult(ctx context.Context, store storage.Store, req AttemptResultRequest) (DeliveryObligation, error) {
	if store == nil {
		return DeliveryObligation{}, typed(ErrInvalidRecordCode, "store is required")
	}
	now := store.Now()
	req.ObligationID = sanitizeID(req.ObligationID)
	req.AttemptRecordID = sanitizeID(req.AttemptRecordID)
	req.ClaimOwner = sanitizeID(req.ClaimOwner)
	req.ProviderIdempotencyKey = sanitizeID(req.ProviderIdempotencyKey)
	req.ResultStatus = sanitizeEnum(req.ResultStatus)
	req.ErrorCode = sanitizeID(req.ErrorCode)
	req.NextAttemptAt = normalizeTimestampOr(req.NextAttemptAt, "")
	req.StartedAt = normalizeTimestampOr(req.StartedAt, canonicalTimestamp(now))
	req.CompletedAt = normalizeTimestampOr(req.CompletedAt, canonicalTimestamp(now))
	req.Evidence = normalizeDeliveryEvidence(req.Evidence)
	if req.ObligationID == "" || req.AttemptRecordID == "" || req.AttemptOrdinal <= 0 ||
		req.ClaimOwner == "" || req.Generation <= 0 || req.ProviderIdempotencyKey == "" {
		return DeliveryObligation{}, typed(ErrInvalidRecordCode, "obligation_id, attempt identity, claim, and idempotency key are required")
	}
	if !attemptResultStatus(req.ResultStatus) {
		return DeliveryObligation{}, typed(ErrInvalidRecordCode, "unsupported attempt result status %q", req.ResultStatus)
	}
	if err := validateAttemptEvidence(req.ResultStatus, req.Evidence); err != nil {
		return DeliveryObligation{}, err
	}
	var stored DeliveryObligation
	err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		obligation, err := loadDeliveryObligationTx(ctx, tx, req.ObligationID)
		if err != nil {
			return err
		}
		attempt, err := loadDeliveryAttemptTx(ctx, tx, req.AttemptRecordID)
		if err != nil {
			return err
		}
		if !deliveryAttemptMatchesResultRequest(attempt, req) {
			return typed(ErrStaleClaimCode, "delivery attempt result does not match prepared attempt authority")
		}
		if result, found, err := loadDeliveryAttemptResultForAttemptTx(ctx, tx, req.AttemptRecordID); err != nil {
			return err
		} else if found {
			if deliveryAttemptResultMatchesRequest(result, req) {
				stored = obligation
				stored.LatestEvidence = result.Evidence
				return nil
			}
			return typed(ErrDuplicateReplayCode, "delivery attempt %s already recorded with different result", req.AttemptRecordID)
		}
		if obligation.Status != DeliveryAttempting && obligation.Status != DeliveryNeedsReconciliation {
			return typed(ErrStaleClaimCode, "obligation %s is not awaiting this attempt result", obligation.ObligationID)
		}
		if obligation.ClaimOwner != req.ClaimOwner || obligation.ClaimGeneration != req.Generation {
			return typed(ErrStaleClaimCode, "obligation %s claim is stale", obligation.ObligationID)
		}
		if obligation.AttemptCount != req.AttemptOrdinal {
			return typed(ErrStaleClaimCode, "obligation %s attempt ordinal is stale", obligation.ObligationID)
		}
		status := req.ResultStatus
		errorCode := req.ErrorCode
		nextAttemptAt := ""
		exhaustedRetry := status == DeliveryRetryableFailure && req.AttemptOrdinal >= obligation.MaxAttempts
		if status == DeliveryRetryableFailure && !exhaustedRetry {
			nextAttemptAt = firstNonEmpty(req.NextAttemptAt, canonicalTimestamp(now.Add(retryDelayForAttempt(req.AttemptOrdinal))))
			if !timestampAfter(nextAttemptAt, now) {
				return typed(ErrInvalidRecordCode, "next_attempt_at must be after now for retryable failure")
			}
		}
		resultID := prefixedDigest("pdelres", map[string]any{
			"schema_version":    SchemaDeliveryResult,
			"attempt_record_id": req.AttemptRecordID,
		})
		evidenceJSON, err := canonicalJSON(req.Evidence)
		if err != nil {
			return err
		}
		payload := map[string]any{
			"schema_version":           SchemaDeliveryResult,
			"record_version":           1,
			"result_record_id":         resultID,
			"attempt_record_id":        req.AttemptRecordID,
			"obligation_id":            req.ObligationID,
			"attempt_ordinal":          req.AttemptOrdinal,
			"claim_owner":              req.ClaimOwner,
			"claim_generation":         req.Generation,
			"transport_contract":       attempt.TransportContract,
			"provider_idempotency_key": req.ProviderIdempotencyKey,
			"result_status":            status,
			"next_attempt_at":          nextAttemptAt,
			"evidence":                 req.Evidence,
			"error_code":               errorCode,
			"started_at":               req.StartedAt,
			"completed_at":             req.CompletedAt,
		}
		payloadJSON, err := canonicalJSON(payload)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO progress_delivery_attempt_results(
				result_record_id, schema_version, record_version, project_id, delivery_run_id, obligation_id,
				attempt_record_id, attempt_ordinal, claim_owner, claim_generation, transport_contract, provider_idempotency_key,
				result_status, next_attempt_at, evidence_kind, evidence_ref, evidence_json, error_code, started_at, completed_at, payload_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			resultID, SchemaDeliveryResult, 1, obligation.ProjectID, obligation.DeliveryRunID, obligation.ObligationID,
			req.AttemptRecordID, req.AttemptOrdinal, req.ClaimOwner, req.Generation, attempt.TransportContract, req.ProviderIdempotencyKey,
			status, nextAttemptAt, req.Evidence.EvidenceKind, req.Evidence.EvidenceRef, string(evidenceJSON), errorCode, req.StartedAt, req.CompletedAt, string(payloadJSON)); err != nil {
			return fmt.Errorf("record delivery attempt result: %w", err)
		}
		obligationStatus := status
		if status == DeliveryDeliveredUnacknowledged && obligation.AckPolicy == DeliveryAckPolicyNone {
			obligationStatus = DeliveryAcknowledged
		}
		if exhaustedRetry {
			obligationStatus = DeliveryTerminalFailure
		}
		updatedAt := canonicalTimestamp(now)
		claimOwner := req.ClaimOwner
		claimedAt := obligation.ClaimedAt
		claimExpiresAt := obligation.ClaimExpiresAt
		if terminalDeliveryStatus(obligationStatus) {
			claimOwner = ""
			claimedAt = ""
			claimExpiresAt = ""
		}
		update, err := tx.Exec(ctx, `UPDATE progress_delivery_obligations
			SET status = ?, claim_owner = ?, claimed_at = ?, claim_expires_at = ?, next_attempt_at = ?, last_error_code = ?, updated_at = ?
			WHERE obligation_id = ? AND status IN (?, ?) AND claim_owner = ? AND claim_generation = ? AND attempt_count = ?`,
			obligationStatus, claimOwner, claimedAt, claimExpiresAt, nextAttemptAt, errorCode, updatedAt,
			req.ObligationID, DeliveryAttempting, DeliveryNeedsReconciliation, req.ClaimOwner, req.Generation, req.AttemptOrdinal)
		if err != nil {
			return fmt.Errorf("update delivery obligation attempt result: %w", err)
		}
		rows, err := update.RowsAffected()
		if err != nil {
			return fmt.Errorf("update delivery obligation attempt result rows affected: %w", err)
		}
		if rows != 1 {
			return typed(ErrStaleClaimCode, "obligation %s attempt result lost active claim fence", obligation.ObligationID)
		}
		stored, err = loadDeliveryObligationTx(ctx, tx, req.ObligationID)
		if err != nil {
			return err
		}
		stored.LatestEvidence = req.Evidence
		return nil
	})
	return stored, err
}

func AcknowledgeDelivery(ctx context.Context, store storage.Store, req AcknowledgmentRequest) (DeliveryObligation, error) {
	if store == nil {
		return DeliveryObligation{}, typed(ErrInvalidRecordCode, "store is required")
	}
	now := store.Now()
	req.ObligationID = sanitizeID(req.ObligationID)
	req.AttemptRecordID = sanitizeID(req.AttemptRecordID)
	req.ResultRecordID = sanitizeID(req.ResultRecordID)
	req.ClaimOwner = sanitizeID(req.ClaimOwner)
	req.ProviderIdempotencyKey = sanitizeID(req.ProviderIdempotencyKey)
	req.Evidence = normalizeDeliveryEvidence(req.Evidence)
	req.AckedAt = normalizeTimestampOr(req.AckedAt, canonicalTimestamp(now))
	if req.ObligationID == "" || req.AttemptRecordID == "" || req.AttemptOrdinal <= 0 || req.ResultRecordID == "" ||
		req.ClaimOwner == "" || req.Generation <= 0 || req.ProviderIdempotencyKey == "" {
		return DeliveryObligation{}, typed(ErrInvalidRecordCode, "obligation_id, attempt/result identity, claim, and idempotency key are required")
	}
	var stored DeliveryObligation
	err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		obligation, err := loadDeliveryObligationTx(ctx, tx, req.ObligationID)
		if err != nil {
			return err
		}
		if obligation.AckPolicy == DeliveryAckPolicyNone {
			return typed(ErrEvidenceRejectedCode, "obligation %s uses no-ack delivery policy", obligation.ObligationID)
		}
		if obligation.Status != DeliveryDeliveredUnacknowledged && obligation.Status != DeliveryAcknowledged {
			return typed(ErrEvidenceRejectedCode, "acknowledgment requires delivered-unacknowledged obligation, got %s", obligation.Status)
		}
		if err := validateAcknowledgmentEvidence(obligation.TransportContract, req.Evidence); err != nil {
			return err
		}
		result, found, err := loadDeliveryAttemptResultByIDTx(ctx, tx, req.ResultRecordID)
		if err != nil {
			return err
		}
		if !found {
			return typed(ErrEvidenceRejectedCode, "delivery result %s not found for acknowledgment", req.ResultRecordID)
		}
		if !deliveryResultMatchesAckRequest(result, req) {
			return typed(ErrStaleClaimCode, "acknowledgment does not match successful delivery result authority")
		}
		if result.ResultStatus != DeliveryDeliveredUnacknowledged {
			return typed(ErrEvidenceRejectedCode, "acknowledgment requires successful delivery result, got %s", result.ResultStatus)
		}
		if result.TransportContract != obligation.TransportContract {
			return typed(ErrEvidenceRejectedCode, "delivery result transport %q does not match obligation contract %q", result.TransportContract, obligation.TransportContract)
		}
		semantic := ackSemanticIdentity(obligation, result, req.Evidence)
		if obligation.Status == DeliveryAcknowledged {
			ack, found, err := loadDeliveryAckByObligationTx(ctx, tx, obligation.ObligationID)
			if err != nil {
				return err
			}
			if !found {
				return typed(ErrEvidenceRejectedCode, "acknowledged obligation %s is missing acknowledgment evidence", obligation.ObligationID)
			}
			if ack.ClaimOwner != req.ClaimOwner || ack.ClaimGeneration != req.Generation {
				return typed(ErrStaleClaimCode, "obligation %s acknowledgment claim is stale", obligation.ObligationID)
			}
			if ack.SemanticIdentity != semantic || ack.Evidence != req.Evidence {
				return typed(ErrDuplicateReplayCode, "obligation %s acknowledgment already recorded with different evidence", obligation.ObligationID)
			}
			stored = obligation
			stored.LatestEvidence = ack.Evidence
			return nil
		}
		if err := requireClaimIdentity(obligation, req.ClaimOwner, req.Generation); err != nil {
			return err
		}
		ackID := prefixedDigest("pdelack", semantic)
		evidenceJSON, err := canonicalJSON(req.Evidence)
		if err != nil {
			return err
		}
		payload := map[string]any{
			"schema_version":           SchemaDeliveryAck,
			"record_version":           1,
			"acknowledgment_id":        ackID,
			"obligation_id":            obligation.ObligationID,
			"semantic_identity":        semantic,
			"attempt_record_id":        req.AttemptRecordID,
			"attempt_ordinal":          req.AttemptOrdinal,
			"result_record_id":         req.ResultRecordID,
			"claim_owner":              req.ClaimOwner,
			"claim_generation":         req.Generation,
			"transport_contract":       obligation.TransportContract,
			"provider_idempotency_key": req.ProviderIdempotencyKey,
			"evidence":                 req.Evidence,
			"acknowledged_at":          req.AckedAt,
		}
		payloadJSON, err := canonicalJSON(payload)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT OR IGNORE INTO progress_delivery_acknowledgments(
				acknowledgment_id, schema_version, record_version, project_id, delivery_run_id, obligation_id,
				semantic_identity, attempt_record_id, attempt_ordinal, result_record_id, claim_owner, claim_generation,
				transport_contract, provider_idempotency_key, evidence_kind, evidence_ref, evidence_json, acknowledged_at, payload_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ackID, SchemaDeliveryAck, 1, obligation.ProjectID, obligation.DeliveryRunID, obligation.ObligationID,
			semantic, req.AttemptRecordID, req.AttemptOrdinal, req.ResultRecordID, req.ClaimOwner, req.Generation,
			obligation.TransportContract, req.ProviderIdempotencyKey, req.Evidence.EvidenceKind,
			req.Evidence.EvidenceRef, string(evidenceJSON), req.AckedAt, string(payloadJSON)); err != nil {
			return fmt.Errorf("record delivery acknowledgment: %w", err)
		}
		updatedAt := canonicalTimestamp(now)
		update, err := tx.Exec(ctx, `UPDATE progress_delivery_obligations
			SET status = ?, claim_owner = '', claimed_at = '', claim_expires_at = '', next_attempt_at = '', updated_at = ?
			WHERE obligation_id = ? AND status = ? AND claim_owner = ? AND claim_generation = ?`,
			DeliveryAcknowledged, updatedAt, req.ObligationID, DeliveryDeliveredUnacknowledged, req.ClaimOwner, req.Generation)
		if err != nil {
			return fmt.Errorf("acknowledge delivery obligation: %w", err)
		}
		rows, err := update.RowsAffected()
		if err != nil {
			return fmt.Errorf("acknowledge delivery obligation rows affected: %w", err)
		}
		if rows != 1 {
			return typed(ErrStaleClaimCode, "obligation %s acknowledgment lost delivery fence", obligation.ObligationID)
		}
		stored, err = loadDeliveryObligationTx(ctx, tx, req.ObligationID)
		if err != nil {
			return err
		}
		stored.LatestEvidence = req.Evidence
		return nil
	})
	return stored, err
}

func AdvanceDeliveryReplayCursor(ctx context.Context, store storage.Store, req CursorAdvanceRequest) error {
	if store == nil {
		return typed(ErrInvalidRecordCode, "store is required")
	}
	now := store.Now()
	req.ObligationID = sanitizeID(req.ObligationID)
	req.ClaimOwner = sanitizeID(req.ClaimOwner)
	req.OriginKind = sanitizeEnum(req.OriginKind)
	req.OriginID = sanitizeID(req.OriginID)
	req.CursorValue = sanitizeText(req.CursorValue, maxTextRunes, nil)
	req.AdvancedAt = normalizeTimestampOr(req.AdvancedAt, canonicalTimestamp(now))
	if req.ObligationID == "" || req.ClaimOwner == "" || req.Generation <= 0 || req.OriginKind == "" || req.OriginID == "" || req.CursorValue == "" {
		return typed(ErrInvalidRecordCode, "obligation, claim, origin, and cursor are required")
	}
	return store.WithWriteTx(ctx, func(tx storage.Tx) error {
		obligation, err := loadDeliveryObligationTx(ctx, tx, req.ObligationID)
		if err != nil {
			return err
		}
		if err := requireCurrentClaim(obligation, req.ClaimOwner, req.Generation, now); err != nil {
			return err
		}
		cursorID := prefixedDigest("pdelcur", map[string]any{
			"project_id":      obligation.ProjectID,
			"delivery_run_id": obligation.DeliveryRunID,
			"origin_kind":     req.OriginKind,
			"origin_id":       req.OriginID,
		})
		payload := map[string]any{
			"schema_version":   SchemaDeliveryCursor,
			"record_version":   1,
			"cursor_id":        cursorID,
			"obligation_id":    obligation.ObligationID,
			"claim_owner":      req.ClaimOwner,
			"claim_generation": req.Generation,
			"origin_kind":      req.OriginKind,
			"origin_id":        req.OriginID,
			"cursor_value":     req.CursorValue,
			"advanced_at":      req.AdvancedAt,
		}
		payloadJSON, err := canonicalJSON(payload)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO progress_delivery_replay_cursors(
				cursor_id, schema_version, record_version, project_id, delivery_run_id, origin_kind, origin_id,
				obligation_id, claim_owner, claim_generation, cursor_value, advanced_at, payload_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(project_id, delivery_run_id, origin_kind, origin_id) DO UPDATE SET
				obligation_id = excluded.obligation_id,
				claim_owner = excluded.claim_owner,
				claim_generation = excluded.claim_generation,
				cursor_value = excluded.cursor_value,
				advanced_at = excluded.advanced_at,
				payload_json = excluded.payload_json`,
			cursorID, SchemaDeliveryCursor, 1, obligation.ProjectID, obligation.DeliveryRunID, req.OriginKind, req.OriginID,
			obligation.ObligationID, req.ClaimOwner, req.Generation, req.CursorValue, req.AdvancedAt, string(payloadJSON))
		if err != nil {
			return fmt.Errorf("advance delivery replay cursor: %w", err)
		}
		return nil
	})
}

func LoadDeliveryObligation(ctx context.Context, store storage.Store, obligationID string) (DeliveryObligation, error) {
	if store == nil {
		return DeliveryObligation{}, typed(ErrInvalidRecordCode, "store is required")
	}
	var obligation DeliveryObligation
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		var err error
		obligation, err = loadDeliveryObligationTx(ctx, tx, sanitizeID(obligationID))
		return err
	})
	return obligation, err
}

func ReadDeliveryObligation(ctx context.Context, store storage.Store, projectID, deliveryRunID, obligationID string) (DeliveryObligation, error) {
	if store == nil {
		return DeliveryObligation{}, typed(ErrInvalidRecordCode, "store is required")
	}
	projectID = strings.TrimSpace(projectID)
	deliveryRunID = strings.TrimSpace(deliveryRunID)
	obligationID = sanitizeID(obligationID)
	if projectID == "" || deliveryRunID == "" || obligationID == "" {
		return DeliveryObligation{}, typed(ErrInvalidRecordCode, "project_id, delivery_run_id, and obligation_id are required")
	}
	var obligation DeliveryObligation
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		var foundProject, foundRun string
		if err := tx.QueryRow(ctx, `SELECT project_id, delivery_run_id FROM progress_delivery_obligations WHERE obligation_id = ?`, obligationID).Scan(&foundProject, &foundRun); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return typed(ErrMissingReferenceCode, "delivery obligation not found")
			}
			return fmt.Errorf("read delivery obligation scope: %w", err)
		}
		if foundProject != projectID || foundRun != deliveryRunID {
			return typed(ErrMissingReferenceCode, "delivery obligation not found in requested project/run")
		}
		var err error
		obligation, err = loadDeliveryObligationTx(ctx, tx, obligationID)
		return err
	})
	return obligation, err
}

func ListDeliveryObligations(ctx context.Context, store storage.Store, filter DeliveryObligationFilter) ([]DeliveryObligation, error) {
	if store == nil {
		return nil, typed(ErrInvalidRecordCode, "store is required")
	}
	projectID := strings.TrimSpace(filter.ProjectID)
	deliveryRunID := strings.TrimSpace(filter.DeliveryRunID)
	if projectID == "" || deliveryRunID == "" {
		return nil, typed(ErrInvalidRecordCode, "project_id and delivery_run_id are required")
	}
	query := `SELECT obligation_id FROM progress_delivery_obligations WHERE project_id = ? AND delivery_run_id = ?`
	args := []any{projectID, deliveryRunID}
	if status := sanitizeEnum(filter.Status); strings.TrimSpace(filter.Status) != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	if sinkKind := sanitizeEnum(filter.SinkKind); strings.TrimSpace(filter.SinkKind) != "" {
		query += ` AND sink_kind = ?`
		args = append(args, sinkKind)
	}
	if sinkID := sanitizeID(filter.SinkID); strings.TrimSpace(filter.SinkID) != "" {
		query += ` AND sink_id = ?`
		args = append(args, sinkID)
	}
	if due := normalizeTimestampOr(filter.DueAtOrBefore, ""); due != "" {
		query += ` AND (next_attempt_at = '' OR next_attempt_at <= ?)`
		args = append(args, due)
	}
	query += ` ORDER BY created_at, obligation_id LIMIT ?`
	args = append(args, boundedLimit(filter.Limit))

	var out []DeliveryObligation
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("list delivery obligations: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("list delivery obligations scan: %w", err)
			}
			obligation, err := loadDeliveryObligationTx(ctx, tx, id)
			if err != nil {
				return err
			}
			out = append(out, obligation)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("list delivery obligations rows: %w", err)
		}
		return nil
	})
	return out, err
}

func ListDeliveryAttempts(ctx context.Context, store storage.Store, filter DeliveryAttemptFilter) ([]DeliveryAttempt, error) {
	if store == nil {
		return nil, typed(ErrInvalidRecordCode, "store is required")
	}
	projectID := strings.TrimSpace(filter.ProjectID)
	deliveryRunID := strings.TrimSpace(filter.DeliveryRunID)
	if projectID == "" || deliveryRunID == "" {
		return nil, typed(ErrInvalidRecordCode, "project_id and delivery_run_id are required")
	}
	query := `SELECT a.payload_json, COALESCE(r.payload_json, '')
		FROM progress_delivery_attempts a
		LEFT JOIN progress_delivery_attempt_results r ON r.attempt_record_id = a.attempt_record_id
		WHERE a.project_id = ? AND a.delivery_run_id = ?`
	args := []any{projectID, deliveryRunID}
	if obligationID := sanitizeID(filter.ObligationID); strings.TrimSpace(filter.ObligationID) != "" {
		query += ` AND a.obligation_id = ?`
		args = append(args, obligationID)
	}
	query += ` ORDER BY a.obligation_id, a.attempt_ordinal, a.attempt_record_id LIMIT ?`
	args = append(args, boundedLimit(filter.Limit))
	out := []DeliveryAttempt{}
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("list delivery attempts: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var payload, resultPayload string
			if err := rows.Scan(&payload, &resultPayload); err != nil {
				return fmt.Errorf("list delivery attempts scan: %w", err)
			}
			attempt, err := decodeDeliveryAttemptPayload([]byte(payload))
			if err != nil {
				return err
			}
			if resultPayload != "" {
				result, err := decodeDeliveryAttemptResultPayload([]byte(resultPayload))
				if err != nil {
					return err
				}
				applyDeliveryAttemptResult(&attempt, result)
			}
			out = append(out, attempt)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("list delivery attempts rows: %w", err)
		}
		return nil
	})
	return out, err
}

func ListDeliveryAcknowledgments(ctx context.Context, store storage.Store, filter DeliveryAckFilter) ([]DeliveryAcknowledgment, error) {
	if store == nil {
		return nil, typed(ErrInvalidRecordCode, "store is required")
	}
	projectID := strings.TrimSpace(filter.ProjectID)
	deliveryRunID := strings.TrimSpace(filter.DeliveryRunID)
	if projectID == "" || deliveryRunID == "" {
		return nil, typed(ErrInvalidRecordCode, "project_id and delivery_run_id are required")
	}
	query := `SELECT payload_json FROM progress_delivery_acknowledgments WHERE project_id = ? AND delivery_run_id = ?`
	args := []any{projectID, deliveryRunID}
	if obligationID := sanitizeID(filter.ObligationID); strings.TrimSpace(filter.ObligationID) != "" {
		query += ` AND obligation_id = ?`
		args = append(args, obligationID)
	}
	query += ` ORDER BY acknowledged_at, acknowledgment_id LIMIT ?`
	args = append(args, boundedLimit(filter.Limit))
	out := []DeliveryAcknowledgment{}
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("list delivery acknowledgments: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				return fmt.Errorf("list delivery acknowledgments scan: %w", err)
			}
			ack, err := decodeDeliveryAckPayload([]byte(payload))
			if err != nil {
				return err
			}
			out = append(out, ack)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("list delivery acknowledgments rows: %w", err)
		}
		return nil
	})
	return out, err
}

func ListDeliveryReplayCursors(ctx context.Context, store storage.Store, filter DeliveryCursorFilter) ([]DeliveryReplayCursor, error) {
	if store == nil {
		return nil, typed(ErrInvalidRecordCode, "store is required")
	}
	projectID := strings.TrimSpace(filter.ProjectID)
	deliveryRunID := strings.TrimSpace(filter.DeliveryRunID)
	if projectID == "" || deliveryRunID == "" {
		return nil, typed(ErrInvalidRecordCode, "project_id and delivery_run_id are required")
	}
	query := `SELECT payload_json FROM progress_delivery_replay_cursors WHERE project_id = ? AND delivery_run_id = ?`
	args := []any{projectID, deliveryRunID}
	if originKind := sanitizeEnum(filter.OriginKind); strings.TrimSpace(filter.OriginKind) != "" {
		query += ` AND origin_kind = ?`
		args = append(args, originKind)
	}
	if originID := sanitizeID(filter.OriginID); strings.TrimSpace(filter.OriginID) != "" {
		query += ` AND origin_id = ?`
		args = append(args, originID)
	}
	query += ` ORDER BY origin_kind, origin_id, cursor_id LIMIT ?`
	args = append(args, boundedLimit(filter.Limit))
	out := []DeliveryReplayCursor{}
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("list delivery replay cursors: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				return fmt.Errorf("list delivery replay cursors scan: %w", err)
			}
			cursor, err := decodeDeliveryCursorPayload([]byte(payload))
			if err != nil {
				return err
			}
			out = append(out, cursor)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("list delivery replay cursors rows: %w", err)
		}
		return nil
	})
	return out, err
}

func persistDeliveryObligationTx(ctx context.Context, tx storage.Tx, obligation DeliveryObligation) (DeliveryObligation, bool, error) {
	candidate, found, err := findDeliveryObligationReplayCandidateTx(ctx, tx, obligation)
	if err != nil {
		return DeliveryObligation{}, false, err
	}
	if found {
		if deliveryObligationImmutableEqual(candidate.Initial, obligation) {
			return candidate.Current, false, nil
		}
		return DeliveryObligation{}, false, typed(ErrDuplicateReplayCode, "delivery obligation replay conflicts with immutable negotiated contract")
	}
	payload, err := canonicalJSON(obligation)
	if err != nil {
		return DeliveryObligation{}, false, typed(ErrInvalidRecordCode, "canonical delivery obligation: %v", err)
	}
	insert, err := tx.Exec(ctx, `INSERT OR IGNORE INTO progress_delivery_obligations(
			obligation_id, schema_version, record_version, project_id, delivery_run_id, progress_receipt_id,
			semantic_identity, origin_kind, origin_id, sink_kind, sink_id, transport_contract, status,
			claim_owner, claim_generation, claimed_at, claim_expires_at, attempt_count, max_attempts,
			next_attempt_at, ack_policy, required_ack, expires_at, superseded_by_obligation_id, last_error_code, created_at, updated_at, payload_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		obligation.ObligationID, obligation.SchemaVersion, obligation.RecordVersion, obligation.ProjectID, obligation.DeliveryRunID,
		obligation.ProgressReceiptID, obligation.SemanticIdentity, obligation.OriginKind, obligation.OriginID, obligation.SinkKind,
		obligation.SinkID, obligation.TransportContract, obligation.Status, obligation.ClaimOwner, obligation.ClaimGeneration,
		obligation.ClaimedAt, obligation.ClaimExpiresAt, obligation.AttemptCount, obligation.MaxAttempts, obligation.NextAttemptAt,
		obligation.AckPolicy, boolInt(obligation.RequiredAck),
		obligation.ExpiresAt, nullIfEmpty(obligation.SupersededByObligationID), obligation.LastErrorCode, obligation.CreatedAt, obligation.UpdatedAt, string(payload))
	if err != nil {
		return DeliveryObligation{}, false, fmt.Errorf("persist delivery obligation: %w", err)
	}
	rows, err := insert.RowsAffected()
	if err != nil {
		return DeliveryObligation{}, false, fmt.Errorf("persist delivery obligation rows affected: %w", err)
	}
	if rows == 0 {
		stored, err := loadDeliveryObligationTx(ctx, tx, obligation.ObligationID)
		if err != nil {
			stored, err = loadDeliveryObligationBySemanticTx(ctx, tx, obligation.ProjectID, obligation.DeliveryRunID, obligation.SemanticIdentity)
		}
		if err != nil {
			return DeliveryObligation{}, false, err
		}
		initial, err := loadDeliveryObligationInitialTx(ctx, tx, stored.ObligationID)
		if err != nil {
			return DeliveryObligation{}, false, err
		}
		if !deliveryObligationImmutableEqual(initial, obligation) {
			return DeliveryObligation{}, false, typed(ErrDuplicateReplayCode, "delivery obligation replay conflicts with immutable negotiated contract")
		}
		return stored, false, nil
	}
	stored, err := loadDeliveryObligationTx(ctx, tx, obligation.ObligationID)
	if err != nil {
		return DeliveryObligation{}, false, err
	}
	return stored, true, nil
}

func normalizeDeliveryObligation(obligation DeliveryObligation, receipt ProgressReceipt, now time.Time) (DeliveryObligation, error) {
	nowText := canonicalTimestamp(now)
	obligation.SchemaVersion = firstNonEmpty(obligation.SchemaVersion, SchemaDeliveryObligation)
	if obligation.SchemaVersion != SchemaDeliveryObligation {
		return DeliveryObligation{}, typed(ErrUnknownRecordVersionCode, "unsupported delivery obligation schema %q", obligation.SchemaVersion)
	}
	if obligation.RecordVersion == 0 {
		obligation.RecordVersion = 1
	}
	if obligation.RecordVersion != 1 {
		return DeliveryObligation{}, typed(ErrUnknownRecordVersionCode, "unsupported delivery obligation record version %d", obligation.RecordVersion)
	}
	obligation.ProjectID = cleanRequiredID("project_id", firstNonEmpty(obligation.ProjectID, receipt.ProjectID))
	obligation.DeliveryRunID = cleanRequiredID("delivery_run_id", firstNonEmpty(obligation.DeliveryRunID, receipt.DeliveryRunID))
	obligation.ProgressReceiptID = sanitizeID(firstNonEmpty(obligation.ProgressReceiptID, receipt.ProgressReceiptID))
	obligation.OriginKind = sanitizeEnum(firstNonEmpty(obligation.OriginKind, "progress-receipt"))
	obligation.OriginID = sanitizeID(firstNonEmpty(obligation.OriginID, receipt.CorrelationID))
	obligation.SinkKind = sanitizeEnum(firstNonEmpty(obligation.SinkKind, "host"))
	obligation.SinkID = sanitizeID(firstNonEmpty(obligation.SinkID, receipt.CorrelationID, Unknown))
	obligation.TransportContract = sanitizeEnum(firstNonEmpty(obligation.TransportContract, Unknown))
	obligation.Status = sanitizeEnum(firstNonEmpty(obligation.Status, DeliveryPending))
	obligation.ClaimOwner = sanitizeID(obligation.ClaimOwner)
	obligation.ClaimedAt = normalizeTimestampOr(obligation.ClaimedAt, "")
	obligation.ClaimExpiresAt = normalizeTimestampOr(obligation.ClaimExpiresAt, "")
	obligation.NextAttemptAt = normalizeTimestampOr(obligation.NextAttemptAt, "")
	obligation.AckPolicy = sanitizeEnum(obligation.AckPolicy)
	obligation.ExpiresAt = normalizeTimestampOr(obligation.ExpiresAt, "")
	obligation.SupersededByObligationID = sanitizeID(obligation.SupersededByObligationID)
	obligation.LastErrorCode = sanitizeID(obligation.LastErrorCode)
	obligation.CreatedAt = normalizeTimestampOr(obligation.CreatedAt, nowText)
	obligation.UpdatedAt = normalizeTimestampOr(obligation.UpdatedAt, nowText)
	if obligation.MaxAttempts == 0 {
		obligation.MaxAttempts = 3
	}
	switch obligation.AckPolicy {
	case "", Unknown:
		obligation.AckPolicy = DeliveryAckPolicyRequired
	case DeliveryAckPolicyRequired, DeliveryAckPolicyNone:
	default:
		return DeliveryObligation{}, typed(ErrInvalidRecordCode, "unsupported delivery ack policy %q", obligation.AckPolicy)
	}
	obligation.RequiredAck = obligation.AckPolicy != DeliveryAckPolicyNone
	if !validDeliveryStatus(obligation.Status) {
		return DeliveryObligation{}, typed(ErrInvalidRecordCode, "unsupported delivery obligation status %q", obligation.Status)
	}
	if obligation.ProjectID == "" || obligation.DeliveryRunID == "" || obligation.ProgressReceiptID == "" ||
		obligation.OriginKind == "" || obligation.OriginID == "" || obligation.SinkKind == "" || obligation.SinkID == "" || obligation.TransportContract == "" {
		return DeliveryObligation{}, typed(ErrInvalidRecordCode, "delivery obligation identity fields are required")
	}
	if obligation.ClaimGeneration < 0 || obligation.AttemptCount < 0 || obligation.MaxAttempts <= 0 {
		return DeliveryObligation{}, typed(ErrInvalidRecordCode, "delivery obligation counters are invalid")
	}
	if obligation.Status == DeliveryRetryableFailure && obligation.NextAttemptAt == "" {
		return DeliveryObligation{}, typed(ErrInvalidRecordCode, "retryable delivery obligation requires next_attempt_at")
	}
	semantic := deliveryObligationSemanticIdentity(obligation)
	obligation.SemanticIdentity = semantic
	if strings.TrimSpace(obligation.ObligationID) == "" {
		obligation.ObligationID = prefixedDigest("pdelobl", semantic)
	} else {
		obligation.ObligationID = sanitizeID(obligation.ObligationID)
	}
	return obligation, nil
}

func normalizeDeliveryEvidence(evidence DeliveryEvidence) DeliveryEvidence {
	evidence.EvidenceKind = sanitizeEnum(evidence.EvidenceKind)
	evidence.EvidenceRef = sanitizeText(evidence.EvidenceRef, maxIdentifierRunes, &RedactionState{})
	evidence.Summary = sanitizeText(evidence.Summary, maxTextRunes, &RedactionState{})
	evidence.Confidence = sanitizeEnum(firstNonEmpty(evidence.Confidence, Unknown))
	evidence.TransportContract = sanitizeEnum(evidence.TransportContract)
	return evidence
}

func validateAttemptEvidence(status string, evidence DeliveryEvidence) error {
	if status == DeliveryDeliveredUnacknowledged && (evidence.EvidenceKind == "" || evidence.EvidenceKind == Unknown || evidence.EvidenceRef == "") {
		return typed(ErrEvidenceRejectedCode, "delivered-unacknowledged attempt requires typed delivery evidence")
	}
	return nil
}

func validateAcknowledgmentEvidence(contract string, evidence DeliveryEvidence) error {
	if evidence.EvidenceKind == "" || evidence.EvidenceKind == Unknown || evidence.EvidenceRef == "" {
		return typed(ErrEvidenceRejectedCode, "acknowledgment requires negotiated typed evidence")
	}
	if evidence.TransportContract != contract {
		return typed(ErrEvidenceRejectedCode, "acknowledgment evidence contract %q does not match obligation contract %q", evidence.TransportContract, contract)
	}
	if evidence.EvidenceKind == "stdout-bytes" && contract != "stdout-ack-v1" {
		return typed(ErrEvidenceRejectedCode, "stdout bytes are not acknowledgment for transport contract %q", contract)
	}
	return nil
}

func requireCurrentClaim(obligation DeliveryObligation, owner string, generation int64, now time.Time) error {
	if err := requireClaimIdentity(obligation, owner, generation); err != nil {
		return err
	}
	if obligation.ClaimExpiresAt == "" || !timestampAfter(obligation.ClaimExpiresAt, now) {
		return typed(ErrStaleClaimCode, "obligation %s claim lease expired", obligation.ObligationID)
	}
	return nil
}

func requireClaimIdentity(obligation DeliveryObligation, owner string, generation int64) error {
	if obligation.ClaimOwner != owner || obligation.ClaimGeneration != generation || generation <= 0 {
		return typed(ErrStaleClaimCode, "obligation %s claim is stale", obligation.ObligationID)
	}
	return nil
}

func loadDeliveryAttemptTx(ctx context.Context, tx storage.Tx, attemptID string) (DeliveryAttempt, error) {
	row := tx.QueryRow(ctx, `SELECT a.payload_json, COALESCE(r.payload_json, '')
		FROM progress_delivery_attempts a
		LEFT JOIN progress_delivery_attempt_results r ON r.attempt_record_id = a.attempt_record_id
		WHERE a.attempt_record_id = ?`, attemptID)
	var payload, resultPayload string
	if err := row.Scan(&payload, &resultPayload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeliveryAttempt{}, typed(ErrMissingReferenceCode, "delivery attempt not found")
		}
		return DeliveryAttempt{}, fmt.Errorf("load delivery attempt: %w", err)
	}
	attempt, err := decodeDeliveryAttemptPayload([]byte(payload))
	if err != nil {
		return DeliveryAttempt{}, err
	}
	if resultPayload != "" {
		result, err := decodeDeliveryAttemptResultPayload([]byte(resultPayload))
		if err != nil {
			return DeliveryAttempt{}, err
		}
		applyDeliveryAttemptResult(&attempt, result)
	}
	return attempt, nil
}

func loadDeliveryAttemptForGenerationTx(ctx context.Context, tx storage.Tx, obligationID string, generation int64) (DeliveryAttempt, bool, error) {
	row := tx.QueryRow(ctx, `SELECT attempt_record_id FROM progress_delivery_attempts
		WHERE obligation_id = ? AND claim_generation = ?
		ORDER BY attempt_ordinal DESC LIMIT 1`, obligationID, generation)
	var attemptID string
	if err := row.Scan(&attemptID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeliveryAttempt{}, false, nil
		}
		return DeliveryAttempt{}, false, fmt.Errorf("load delivery attempt replay: %w", err)
	}
	attempt, err := loadDeliveryAttemptTx(ctx, tx, attemptID)
	if err != nil {
		return DeliveryAttempt{}, false, err
	}
	return attempt, true, nil
}

func loadUnresolvedDeliveryAttemptTx(ctx context.Context, tx storage.Tx, obligationID string) (DeliveryAttempt, bool, error) {
	row := tx.QueryRow(ctx, `SELECT a.attempt_record_id FROM progress_delivery_attempts a
		LEFT JOIN progress_delivery_attempt_results r ON r.attempt_record_id = a.attempt_record_id
		WHERE a.obligation_id = ? AND r.result_record_id IS NULL
		ORDER BY a.attempt_ordinal DESC LIMIT 1`, obligationID)
	var attemptID string
	if err := row.Scan(&attemptID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeliveryAttempt{}, false, nil
		}
		return DeliveryAttempt{}, false, fmt.Errorf("load unresolved delivery attempt: %w", err)
	}
	attempt, err := loadDeliveryAttemptTx(ctx, tx, attemptID)
	if err != nil {
		return DeliveryAttempt{}, false, err
	}
	return attempt, true, nil
}

func loadDeliveryAttemptResultForAttemptTx(ctx context.Context, tx storage.Tx, attemptID string) (DeliveryAttemptResult, bool, error) {
	row := tx.QueryRow(ctx, `SELECT payload_json FROM progress_delivery_attempt_results WHERE attempt_record_id = ?`, attemptID)
	var payload string
	if err := row.Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeliveryAttemptResult{}, false, nil
		}
		return DeliveryAttemptResult{}, false, fmt.Errorf("load delivery attempt result: %w", err)
	}
	result, err := decodeDeliveryAttemptResultPayload([]byte(payload))
	return result, true, err
}

func loadDeliveryAttemptResultByIDTx(ctx context.Context, tx storage.Tx, resultID string) (DeliveryAttemptResult, bool, error) {
	row := tx.QueryRow(ctx, `SELECT payload_json FROM progress_delivery_attempt_results WHERE result_record_id = ?`, resultID)
	var payload string
	if err := row.Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeliveryAttemptResult{}, false, nil
		}
		return DeliveryAttemptResult{}, false, fmt.Errorf("load delivery attempt result by id: %w", err)
	}
	result, err := decodeDeliveryAttemptResultPayload([]byte(payload))
	return result, true, err
}

func loadDeliveryAckByObligationTx(ctx context.Context, tx storage.Tx, obligationID string) (DeliveryAcknowledgment, bool, error) {
	row := tx.QueryRow(ctx, `SELECT payload_json FROM progress_delivery_acknowledgments
		WHERE obligation_id = ?
		ORDER BY acknowledged_at, acknowledgment_id LIMIT 1`, obligationID)
	var payload string
	if err := row.Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeliveryAcknowledgment{}, false, nil
		}
		return DeliveryAcknowledgment{}, false, fmt.Errorf("load delivery acknowledgment replay: %w", err)
	}
	ack, err := decodeDeliveryAckPayload([]byte(payload))
	if err != nil {
		return DeliveryAcknowledgment{}, false, err
	}
	return ack, true, nil
}

func deliveryAttemptMatchesResultRequest(attempt DeliveryAttempt, req AttemptResultRequest) bool {
	return attempt.ObligationID == req.ObligationID &&
		attempt.AttemptRecordID == req.AttemptRecordID &&
		attempt.AttemptOrdinal == req.AttemptOrdinal &&
		attempt.ClaimOwner == req.ClaimOwner &&
		attempt.ClaimGeneration == req.Generation &&
		attempt.ProviderIdempotencyKey == req.ProviderIdempotencyKey
}

func deliveryAttemptResultMatchesRequest(result DeliveryAttemptResult, req AttemptResultRequest) bool {
	if result.ObligationID != req.ObligationID || result.AttemptRecordID != req.AttemptRecordID ||
		result.AttemptOrdinal != req.AttemptOrdinal || result.ClaimOwner != req.ClaimOwner ||
		result.ClaimGeneration != req.Generation || result.ProviderIdempotencyKey != req.ProviderIdempotencyKey {
		return false
	}
	if result.ResultStatus != req.ResultStatus || result.ErrorCode != req.ErrorCode || result.Evidence != req.Evidence {
		return false
	}
	return req.NextAttemptAt == "" || result.NextAttemptAt == req.NextAttemptAt
}

func deliveryResultMatchesAckRequest(result DeliveryAttemptResult, req AcknowledgmentRequest) bool {
	return result.ObligationID == req.ObligationID &&
		result.AttemptRecordID == req.AttemptRecordID &&
		result.AttemptOrdinal == req.AttemptOrdinal &&
		result.ResultRecordID == req.ResultRecordID &&
		result.ClaimOwner == req.ClaimOwner &&
		result.ClaimGeneration == req.Generation &&
		result.ProviderIdempotencyKey == req.ProviderIdempotencyKey
}

func applyDeliveryAttemptResult(attempt *DeliveryAttempt, result DeliveryAttemptResult) {
	attempt.ResultRecordID = result.ResultRecordID
	attempt.ResultStatus = result.ResultStatus
	attempt.NextAttemptAt = result.NextAttemptAt
	attempt.Evidence = result.Evidence
	attempt.ErrorCode = result.ErrorCode
	attempt.StartedAt = result.StartedAt
	attempt.CompletedAt = result.CompletedAt
}

func loadDeliveryObligationTx(ctx context.Context, tx storage.Tx, obligationID string) (DeliveryObligation, error) {
	if obligationID == "" {
		return DeliveryObligation{}, typed(ErrInvalidRecordCode, "obligation_id is required")
	}
	row := tx.QueryRow(ctx, `SELECT payload_json, status, claim_owner, claim_generation, claimed_at, claim_expires_at,
			attempt_count, next_attempt_at, ack_policy, required_ack, last_error_code, updated_at
		FROM progress_delivery_obligations WHERE obligation_id = ?`, obligationID)
	var payload string
	var status, owner, claimedAt, expiresAt, nextAttemptAt, ackPolicy, lastError, updatedAt string
	var generation int64
	var attempts, requiredAck int
	if err := row.Scan(&payload, &status, &owner, &generation, &claimedAt, &expiresAt, &attempts, &nextAttemptAt, &ackPolicy, &requiredAck, &lastError, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeliveryObligation{}, typed(ErrMissingReferenceCode, "delivery obligation not found")
		}
		return DeliveryObligation{}, fmt.Errorf("load delivery obligation: %w", err)
	}
	obligation, err := decodeDeliveryObligationPayload([]byte(payload))
	if err != nil {
		return DeliveryObligation{}, err
	}
	obligation.Status = status
	obligation.ClaimOwner = owner
	obligation.ClaimGeneration = generation
	obligation.ClaimedAt = claimedAt
	obligation.ClaimExpiresAt = expiresAt
	obligation.AttemptCount = attempts
	obligation.NextAttemptAt = nextAttemptAt
	obligation.AckPolicy = ackPolicy
	obligation.RequiredAck = requiredAck == 1
	obligation.LastErrorCode = lastError
	obligation.UpdatedAt = updatedAt
	return obligation, nil
}

func loadDeliveryObligationInitialTx(ctx context.Context, tx storage.Tx, obligationID string) (DeliveryObligation, error) {
	if obligationID == "" {
		return DeliveryObligation{}, typed(ErrInvalidRecordCode, "obligation_id is required")
	}
	var payload string
	if err := tx.QueryRow(ctx, `SELECT payload_json FROM progress_delivery_obligations WHERE obligation_id = ?`, obligationID).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeliveryObligation{}, typed(ErrMissingReferenceCode, "delivery obligation not found")
		}
		return DeliveryObligation{}, fmt.Errorf("load initial delivery obligation: %w", err)
	}
	return decodeDeliveryObligationPayload([]byte(payload))
}

func loadDeliveryObligationBySemanticTx(ctx context.Context, tx storage.Tx, projectID, deliveryRunID, semantic string) (DeliveryObligation, error) {
	var obligationID string
	if err := tx.QueryRow(ctx, `SELECT obligation_id FROM progress_delivery_obligations
		WHERE project_id = ? AND delivery_run_id = ? AND semantic_identity = ?`,
		projectID, deliveryRunID, semantic).Scan(&obligationID); err != nil {
		return DeliveryObligation{}, err
	}
	return loadDeliveryObligationTx(ctx, tx, obligationID)
}

func findDeliveryObligationReplayCandidateTx(ctx context.Context, tx storage.Tx, obligation DeliveryObligation) (deliveryObligationReplayCandidate, bool, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT obligation_id FROM progress_delivery_obligations
		WHERE project_id = ? AND delivery_run_id = ? AND progress_receipt_id = ? AND origin_kind = ? AND origin_id = ?
		ORDER BY created_at, obligation_id LIMIT 1`,
		obligation.ProjectID, obligation.DeliveryRunID, obligation.ProgressReceiptID, obligation.OriginKind, obligation.OriginID).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return deliveryObligationReplayCandidate{}, false, nil
		}
		return deliveryObligationReplayCandidate{}, false, fmt.Errorf("find delivery obligation replay: %w", err)
	}
	current, err := loadDeliveryObligationTx(ctx, tx, id)
	if err != nil {
		return deliveryObligationReplayCandidate{}, false, err
	}
	initial, err := loadDeliveryObligationInitialTx(ctx, tx, id)
	if err != nil {
		return deliveryObligationReplayCandidate{}, false, err
	}
	return deliveryObligationReplayCandidate{Current: current, Initial: initial}, true, nil
}

func deliveryObligationImmutableEqual(a, b DeliveryObligation) bool {
	return a.SchemaVersion == b.SchemaVersion &&
		a.RecordVersion == b.RecordVersion &&
		a.ProjectID == b.ProjectID &&
		a.DeliveryRunID == b.DeliveryRunID &&
		a.ProgressReceiptID == b.ProgressReceiptID &&
		a.OriginKind == b.OriginKind &&
		a.OriginID == b.OriginID &&
		a.SinkKind == b.SinkKind &&
		a.SinkID == b.SinkID &&
		a.TransportContract == b.TransportContract &&
		a.Status == b.Status &&
		a.MaxAttempts == b.MaxAttempts &&
		a.AckPolicy == b.AckPolicy &&
		a.RequiredAck == b.RequiredAck &&
		a.ExpiresAt == b.ExpiresAt &&
		a.SupersededByObligationID == b.SupersededByObligationID &&
		a.NextAttemptAt == b.NextAttemptAt
}

func decodeDeliveryObligationPayload(data []byte) (DeliveryObligation, error) {
	var obligation DeliveryObligation
	if err := json.Unmarshal(data, &obligation); err != nil {
		return DeliveryObligation{}, typed(ErrInvalidRecordCode, "decode persisted delivery obligation: %v", err)
	}
	if obligation.SchemaVersion != SchemaDeliveryObligation || obligation.RecordVersion != 1 {
		return DeliveryObligation{}, typed(ErrUnknownRecordVersionCode, "unsupported delivery obligation %q record version %d", obligation.SchemaVersion, obligation.RecordVersion)
	}
	return obligation, nil
}

func decodeDeliveryAttemptPayload(data []byte) (DeliveryAttempt, error) {
	var attempt DeliveryAttempt
	if err := json.Unmarshal(data, &attempt); err != nil {
		return DeliveryAttempt{}, typed(ErrInvalidRecordCode, "decode persisted delivery attempt: %v", err)
	}
	if attempt.SchemaVersion != SchemaDeliveryAttempt || attempt.RecordVersion != 1 {
		return DeliveryAttempt{}, typed(ErrUnknownRecordVersionCode, "unsupported delivery attempt %q record version %d", attempt.SchemaVersion, attempt.RecordVersion)
	}
	return attempt, nil
}

func decodeDeliveryAttemptResultPayload(data []byte) (DeliveryAttemptResult, error) {
	var result DeliveryAttemptResult
	if err := json.Unmarshal(data, &result); err != nil {
		return DeliveryAttemptResult{}, typed(ErrInvalidRecordCode, "decode persisted delivery attempt result: %v", err)
	}
	if result.SchemaVersion != SchemaDeliveryResult || result.RecordVersion != 1 {
		return DeliveryAttemptResult{}, typed(ErrUnknownRecordVersionCode, "unsupported delivery attempt result %q record version %d", result.SchemaVersion, result.RecordVersion)
	}
	return result, nil
}

func decodeDeliveryAckPayload(data []byte) (DeliveryAcknowledgment, error) {
	var ack DeliveryAcknowledgment
	if err := json.Unmarshal(data, &ack); err != nil {
		return DeliveryAcknowledgment{}, typed(ErrInvalidRecordCode, "decode persisted delivery acknowledgment: %v", err)
	}
	if ack.SchemaVersion != SchemaDeliveryAck || ack.RecordVersion != 1 {
		return DeliveryAcknowledgment{}, typed(ErrUnknownRecordVersionCode, "unsupported delivery acknowledgment %q record version %d", ack.SchemaVersion, ack.RecordVersion)
	}
	return ack, nil
}

func decodeDeliveryCursorPayload(data []byte) (DeliveryReplayCursor, error) {
	var cursor DeliveryReplayCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return DeliveryReplayCursor{}, typed(ErrInvalidRecordCode, "decode persisted delivery replay cursor: %v", err)
	}
	if cursor.SchemaVersion != SchemaDeliveryCursor || cursor.RecordVersion != 1 {
		return DeliveryReplayCursor{}, typed(ErrUnknownRecordVersionCode, "unsupported delivery replay cursor %q record version %d", cursor.SchemaVersion, cursor.RecordVersion)
	}
	return cursor, nil
}

func deliveryObligationSemanticIdentity(obligation DeliveryObligation) string {
	return prefixedDigest("sha256", map[string]any{
		"schema_version":      SchemaDeliveryObligation,
		"project_id":          obligation.ProjectID,
		"delivery_run_id":     obligation.DeliveryRunID,
		"progress_receipt_id": obligation.ProgressReceiptID,
		"origin_kind":         obligation.OriginKind,
		"origin_id":           obligation.OriginID,
		"sink_kind":           obligation.SinkKind,
		"sink_id":             obligation.SinkID,
		"transport_contract":  obligation.TransportContract,
		"status":              obligation.Status,
		"max_attempts":        obligation.MaxAttempts,
		"ack_policy":          obligation.AckPolicy,
		"required_ack":        obligation.RequiredAck,
		"expires_at":          obligation.ExpiresAt,
		"superseded_by":       obligation.SupersededByObligationID,
		"next_attempt_at":     obligation.NextAttemptAt,
	})
}

func ackSemanticIdentity(obligation DeliveryObligation, result DeliveryAttemptResult, evidence DeliveryEvidence) string {
	return prefixedDigest("sha256", map[string]any{
		"schema_version":      SchemaDeliveryAck,
		"obligation_identity": obligation.SemanticIdentity,
		"attempt_record_id":   result.AttemptRecordID,
		"result_record_id":    result.ResultRecordID,
		"transport_contract":  obligation.TransportContract,
		"evidence_kind":       evidence.EvidenceKind,
		"evidence_ref":        evidence.EvidenceRef,
	})
}

func prefixedDigest(prefix string, v any) string {
	digest, _, err := digestCanonicalJSON(v)
	if err != nil {
		return prefix + "_invalid"
	}
	hex := strings.TrimPrefix(digest, "sha256:")
	if prefix == "sha256" {
		return "sha256:" + hex
	}
	return prefix + "_" + hex[:40]
}

func validDeliveryStatus(status string) bool {
	switch status {
	case DeliveryPending, DeliveryAttempting, DeliveryNeedsReconciliation, DeliveryDeliveredUnacknowledged, DeliveryAcknowledged, DeliveryUnsupported, DeliveryRetryableFailure, DeliveryTerminalFailure, DeliveryExpired, DeliverySuperseded:
		return true
	default:
		return false
	}
}

func attemptResultStatus(status string) bool {
	switch status {
	case DeliveryDeliveredUnacknowledged, DeliveryUnsupported, DeliveryRetryableFailure, DeliveryTerminalFailure, DeliveryExpired, DeliverySuperseded:
		return true
	default:
		return false
	}
}

func terminalDeliveryStatus(status string) bool {
	switch status {
	case DeliveryAcknowledged, DeliveryUnsupported, DeliveryTerminalFailure, DeliveryExpired, DeliverySuperseded:
		return true
	default:
		return false
	}
}

func retryDelayForAttempt(attempt int) time.Duration {
	if attempt <= 1 {
		return 15 * time.Second
	}
	delay := 15 * time.Second
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= 5*time.Minute {
			return 5 * time.Minute
		}
	}
	return delay
}

func timestampAfter(value string, now time.Time) bool {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return false
	}
	return parsed.UTC().After(now.UTC())
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nullIfEmpty(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
