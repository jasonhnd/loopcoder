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
	SchemaDeliveryAck        = "loopcoder.progress_delivery_acknowledgment.v1"
	SchemaDeliveryCursor     = "loopcoder.progress_delivery_replay_cursor.v1"

	DeliveryPending                 = "pending"
	DeliveryAttempting              = "attempting"
	DeliveryDeliveredUnacknowledged = "delivered-unacknowledged"
	DeliveryAcknowledged            = "acknowledged"
	DeliveryUnsupported             = "unsupported"
	DeliveryRetryableFailure        = "retryable-failure"
	DeliveryTerminalFailure         = "terminal-failure"
	DeliveryExpired                 = "expired"
	DeliverySuperseded              = "superseded"
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
	RequiredAck              bool             `json:"required_ack"`
	ExpiresAt                string           `json:"expires_at,omitempty"`
	SupersededByObligationID string           `json:"superseded_by_obligation_id,omitempty"`
	LastErrorCode            string           `json:"last_error_code,omitempty"`
	CreatedAt                string           `json:"created_at"`
	UpdatedAt                string           `json:"updated_at"`
	LatestEvidence           DeliveryEvidence `json:"latest_evidence,omitempty"`
}

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
	ObligationID string
	ClaimOwner   string
	Generation   int64
	ResultStatus string
	Evidence     DeliveryEvidence
	ErrorCode    string
	StartedAt    string
	CompletedAt  string
}

type AcknowledgmentRequest struct {
	ObligationID string
	ClaimOwner   string
	Generation   int64
	Evidence     DeliveryEvidence
	AckedAt      string
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

func PersistReceiptWithObligation(ctx context.Context, store storage.Store, receipt ProgressReceipt, obligation DeliveryObligation) (PersistReceiptWithObligationResult, error) {
	if store == nil {
		return PersistReceiptWithObligationResult{}, typed(ErrInvalidRecordCode, "store is required")
	}
	now := store.Now()
	normalizedReceipt, err := NormalizeReceipt(receipt, now)
	if err != nil {
		return PersistReceiptWithObligationResult{}, err
	}
	normalizedObligation, err := normalizeDeliveryObligation(obligation, normalizedReceipt, now)
	if err != nil {
		return PersistReceiptWithObligationResult{}, err
	}
	var result PersistReceiptWithObligationResult
	err = store.WithWriteTx(ctx, func(tx storage.Tx) error {
		receiptResult, err := persistNormalizedReceiptTx(ctx, tx, normalizedReceipt, now, false)
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
	err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		obligation, err := loadDeliveryObligationTx(ctx, tx, obligationID)
		if err != nil {
			return err
		}
		if terminalDeliveryStatus(obligation.Status) {
			return typed(ErrClaimConflictCode, "obligation %s is terminal in state %s", obligationID, obligation.Status)
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
	return result, err
}

func RecordDeliveryAttemptResult(ctx context.Context, store storage.Store, req AttemptResultRequest) (DeliveryObligation, error) {
	if store == nil {
		return DeliveryObligation{}, typed(ErrInvalidRecordCode, "store is required")
	}
	now := store.Now()
	req.ObligationID = sanitizeID(req.ObligationID)
	req.ClaimOwner = sanitizeID(req.ClaimOwner)
	req.ResultStatus = sanitizeEnum(req.ResultStatus)
	req.ErrorCode = sanitizeID(req.ErrorCode)
	req.StartedAt = normalizeTimestampOr(req.StartedAt, canonicalTimestamp(now))
	req.CompletedAt = normalizeTimestampOr(req.CompletedAt, canonicalTimestamp(now))
	req.Evidence = normalizeDeliveryEvidence(req.Evidence)
	if req.ObligationID == "" || req.ClaimOwner == "" || req.Generation <= 0 {
		return DeliveryObligation{}, typed(ErrInvalidRecordCode, "obligation_id, claim_owner, and generation are required")
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
		if err := requireCurrentClaim(obligation, req.ClaimOwner, req.Generation, now); err != nil {
			return err
		}
		nextAttempt := obligation.AttemptCount + 1
		status := req.ResultStatus
		errorCode := req.ErrorCode
		if nextAttempt > obligation.MaxAttempts {
			updatedAt := canonicalTimestamp(now)
			if _, err := tx.Exec(ctx, `UPDATE progress_delivery_obligations
				SET status = ?, last_error_code = ?, updated_at = ?
				WHERE obligation_id = ? AND claim_owner = ? AND claim_generation = ?`,
				DeliveryTerminalFailure, firstNonEmpty(errorCode, "max-attempts-exceeded"), updatedAt, req.ObligationID, req.ClaimOwner, req.Generation); err != nil {
				return fmt.Errorf("record bounded delivery attempt failure: %w", err)
			}
			stored, err = loadDeliveryObligationTx(ctx, tx, req.ObligationID)
			return err
		}
		attemptID := prefixedDigest("pdelatt", map[string]any{
			"obligation_id": req.ObligationID,
			"attempt":       nextAttempt,
			"generation":    req.Generation,
		})
		evidenceJSON, err := canonicalJSON(req.Evidence)
		if err != nil {
			return err
		}
		payload := map[string]any{
			"schema_version":    SchemaDeliveryAttempt,
			"record_version":    1,
			"attempt_record_id": attemptID,
			"obligation_id":     req.ObligationID,
			"attempt_ordinal":   nextAttempt,
			"claim_owner":       req.ClaimOwner,
			"claim_generation":  req.Generation,
			"result_status":     status,
			"evidence":          req.Evidence,
			"error_code":        errorCode,
			"started_at":        req.StartedAt,
			"completed_at":      req.CompletedAt,
		}
		payloadJSON, err := canonicalJSON(payload)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO progress_delivery_attempts(
				attempt_record_id, schema_version, record_version, project_id, delivery_run_id, obligation_id,
				attempt_ordinal, claim_owner, claim_generation, result_status, evidence_kind, evidence_ref,
				evidence_json, error_code, started_at, completed_at, payload_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			attemptID, SchemaDeliveryAttempt, 1, obligation.ProjectID, obligation.DeliveryRunID, obligation.ObligationID,
			nextAttempt, req.ClaimOwner, req.Generation, status, req.Evidence.EvidenceKind, req.Evidence.EvidenceRef,
			string(evidenceJSON), errorCode, req.StartedAt, req.CompletedAt, string(payloadJSON)); err != nil {
			return fmt.Errorf("record delivery attempt: %w", err)
		}
		updatedAt := canonicalTimestamp(now)
		if _, err := tx.Exec(ctx, `UPDATE progress_delivery_obligations
			SET status = ?, attempt_count = ?, last_error_code = ?, updated_at = ?
			WHERE obligation_id = ? AND claim_owner = ? AND claim_generation = ?`,
			status, nextAttempt, errorCode, updatedAt, req.ObligationID, req.ClaimOwner, req.Generation); err != nil {
			return fmt.Errorf("update delivery obligation attempt result: %w", err)
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
	req.ClaimOwner = sanitizeID(req.ClaimOwner)
	req.Evidence = normalizeDeliveryEvidence(req.Evidence)
	req.AckedAt = normalizeTimestampOr(req.AckedAt, canonicalTimestamp(now))
	if req.ObligationID == "" || req.ClaimOwner == "" || req.Generation <= 0 {
		return DeliveryObligation{}, typed(ErrInvalidRecordCode, "obligation_id, claim_owner, and generation are required")
	}
	var stored DeliveryObligation
	err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		obligation, err := loadDeliveryObligationTx(ctx, tx, req.ObligationID)
		if err != nil {
			return err
		}
		if err := requireCurrentClaim(obligation, req.ClaimOwner, req.Generation, now); err != nil {
			return err
		}
		if obligation.Status != DeliveryDeliveredUnacknowledged && obligation.Status != DeliveryAcknowledged {
			return typed(ErrEvidenceRejectedCode, "acknowledgment requires delivered-unacknowledged obligation, got %s", obligation.Status)
		}
		if err := validateAcknowledgmentEvidence(obligation.TransportContract, req.Evidence); err != nil {
			return err
		}
		semantic := ackSemanticIdentity(obligation, req.Evidence)
		ackID := prefixedDigest("pdelack", semantic)
		evidenceJSON, err := canonicalJSON(req.Evidence)
		if err != nil {
			return err
		}
		payload := map[string]any{
			"schema_version":     SchemaDeliveryAck,
			"record_version":     1,
			"acknowledgment_id":  ackID,
			"obligation_id":      obligation.ObligationID,
			"semantic_identity":  semantic,
			"claim_owner":        req.ClaimOwner,
			"claim_generation":   req.Generation,
			"transport_contract": obligation.TransportContract,
			"evidence":           req.Evidence,
			"acknowledged_at":    req.AckedAt,
		}
		payloadJSON, err := canonicalJSON(payload)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT OR IGNORE INTO progress_delivery_acknowledgments(
				acknowledgment_id, schema_version, record_version, project_id, delivery_run_id, obligation_id,
				semantic_identity, claim_owner, claim_generation, transport_contract, evidence_kind, evidence_ref,
				evidence_json, acknowledged_at, payload_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ackID, SchemaDeliveryAck, 1, obligation.ProjectID, obligation.DeliveryRunID, obligation.ObligationID,
			semantic, req.ClaimOwner, req.Generation, obligation.TransportContract, req.Evidence.EvidenceKind,
			req.Evidence.EvidenceRef, string(evidenceJSON), req.AckedAt, string(payloadJSON)); err != nil {
			return fmt.Errorf("record delivery acknowledgment: %w", err)
		}
		updatedAt := canonicalTimestamp(now)
		if _, err := tx.Exec(ctx, `UPDATE progress_delivery_obligations
			SET status = ?, updated_at = ?
			WHERE obligation_id = ? AND claim_owner = ? AND claim_generation = ?`,
			DeliveryAcknowledged, updatedAt, req.ObligationID, req.ClaimOwner, req.Generation); err != nil {
			return fmt.Errorf("acknowledge delivery obligation: %w", err)
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

func persistDeliveryObligationTx(ctx context.Context, tx storage.Tx, obligation DeliveryObligation) (DeliveryObligation, bool, error) {
	payload, err := canonicalJSON(obligation)
	if err != nil {
		return DeliveryObligation{}, false, typed(ErrInvalidRecordCode, "canonical delivery obligation: %v", err)
	}
	insert, err := tx.Exec(ctx, `INSERT OR IGNORE INTO progress_delivery_obligations(
			obligation_id, schema_version, record_version, project_id, delivery_run_id, progress_receipt_id,
			semantic_identity, origin_kind, origin_id, sink_kind, sink_id, transport_contract, status,
			claim_owner, claim_generation, claimed_at, claim_expires_at, attempt_count, max_attempts,
			required_ack, expires_at, superseded_by_obligation_id, last_error_code, created_at, updated_at, payload_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		obligation.ObligationID, obligation.SchemaVersion, obligation.RecordVersion, obligation.ProjectID, obligation.DeliveryRunID,
		obligation.ProgressReceiptID, obligation.SemanticIdentity, obligation.OriginKind, obligation.OriginID, obligation.SinkKind,
		obligation.SinkID, obligation.TransportContract, obligation.Status, obligation.ClaimOwner, obligation.ClaimGeneration,
		obligation.ClaimedAt, obligation.ClaimExpiresAt, obligation.AttemptCount, obligation.MaxAttempts, boolInt(obligation.RequiredAck),
		obligation.ExpiresAt, nullIfEmpty(obligation.SupersededByObligationID), obligation.LastErrorCode, obligation.CreatedAt, obligation.UpdatedAt, string(payload))
	if err != nil {
		return DeliveryObligation{}, false, fmt.Errorf("persist delivery obligation: %w", err)
	}
	rows, err := insert.RowsAffected()
	if err != nil {
		return DeliveryObligation{}, false, fmt.Errorf("persist delivery obligation rows affected: %w", err)
	}
	stored, err := loadDeliveryObligationBySemanticTx(ctx, tx, obligation.ProjectID, obligation.DeliveryRunID, obligation.SemanticIdentity)
	if err != nil {
		return DeliveryObligation{}, false, err
	}
	return stored, rows == 1, nil
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
	obligation.SinkID = sanitizeID(firstNonEmpty(obligation.SinkID, Unknown))
	obligation.TransportContract = sanitizeEnum(firstNonEmpty(obligation.TransportContract, Unknown))
	obligation.Status = sanitizeEnum(firstNonEmpty(obligation.Status, DeliveryPending))
	obligation.ClaimOwner = sanitizeID(obligation.ClaimOwner)
	obligation.ClaimedAt = normalizeTimestampOr(obligation.ClaimedAt, "")
	obligation.ClaimExpiresAt = normalizeTimestampOr(obligation.ClaimExpiresAt, "")
	obligation.ExpiresAt = normalizeTimestampOr(obligation.ExpiresAt, "")
	obligation.SupersededByObligationID = sanitizeID(obligation.SupersededByObligationID)
	obligation.LastErrorCode = sanitizeID(obligation.LastErrorCode)
	obligation.CreatedAt = normalizeTimestampOr(obligation.CreatedAt, nowText)
	obligation.UpdatedAt = normalizeTimestampOr(obligation.UpdatedAt, nowText)
	if obligation.MaxAttempts == 0 {
		obligation.MaxAttempts = 3
	}
	if !obligation.RequiredAck {
		obligation.RequiredAck = true
	}
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
	if obligation.ClaimOwner != owner || obligation.ClaimGeneration != generation || generation <= 0 {
		return typed(ErrStaleClaimCode, "obligation %s claim is stale", obligation.ObligationID)
	}
	if obligation.ClaimExpiresAt == "" || !timestampAfter(obligation.ClaimExpiresAt, now) {
		return typed(ErrStaleClaimCode, "obligation %s claim lease expired", obligation.ObligationID)
	}
	return nil
}

func loadDeliveryObligationTx(ctx context.Context, tx storage.Tx, obligationID string) (DeliveryObligation, error) {
	if obligationID == "" {
		return DeliveryObligation{}, typed(ErrInvalidRecordCode, "obligation_id is required")
	}
	row := tx.QueryRow(ctx, `SELECT payload_json, status, claim_owner, claim_generation, claimed_at, claim_expires_at,
			attempt_count, last_error_code, updated_at
		FROM progress_delivery_obligations WHERE obligation_id = ?`, obligationID)
	var payload string
	var status, owner, claimedAt, expiresAt, lastError, updatedAt string
	var generation int64
	var attempts int
	if err := row.Scan(&payload, &status, &owner, &generation, &claimedAt, &expiresAt, &attempts, &lastError, &updatedAt); err != nil {
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
	obligation.LastErrorCode = lastError
	obligation.UpdatedAt = updatedAt
	return obligation, nil
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
	})
}

func ackSemanticIdentity(obligation DeliveryObligation, evidence DeliveryEvidence) string {
	return prefixedDigest("sha256", map[string]any{
		"schema_version":      SchemaDeliveryAck,
		"obligation_identity": obligation.SemanticIdentity,
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
	case DeliveryPending, DeliveryAttempting, DeliveryDeliveredUnacknowledged, DeliveryAcknowledged, DeliveryUnsupported, DeliveryRetryableFailure, DeliveryTerminalFailure, DeliveryExpired, DeliverySuperseded:
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
