package progress

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/storage"
)

const (
	DefaultHostReplayLimit = 50
	MaxHostReplayLimit     = 500
)

type HostReplayOptions struct {
	ProjectID     string
	DeliveryRunID string
	OriginKind    string
	OriginID      string
	ClaimOwner    string
	LeaseDuration time.Duration
	Limit         int
	Now           time.Time
}

type HostReplayResult struct {
	Views        []ReceiptView `json:"receipts"`
	Replayed     int           `json:"replayed"`
	NextCursor   string        `json:"next_cursor,omitempty"`
	OriginKind   string        `json:"origin_kind"`
	OriginID     string        `json:"origin_id"`
	DeliveryOpen bool          `json:"delivery_open"`
}

func ReplayPendingForHost(ctx context.Context, store storage.Store, opts HostReplayOptions, emit func(ReceiptView) error) (HostReplayResult, error) {
	if store == nil {
		return HostReplayResult{}, typed(ErrInvalidRecordCode, "store is required")
	}
	if emit == nil {
		return HostReplayResult{}, typed(ErrInvalidRecordCode, "emit callback is required")
	}
	now := opts.Now.UTC()
	if now.IsZero() {
		now = store.Now().UTC()
	}
	projectID := strings.TrimSpace(opts.ProjectID)
	deliveryRunID := strings.TrimSpace(opts.DeliveryRunID)
	originKind := sanitizeEnum(opts.OriginKind)
	originID := sanitizeID(opts.OriginID)
	owner := sanitizeID(firstNonEmpty(opts.ClaimOwner, "host-replay"))
	if projectID == "" || deliveryRunID == "" || originKind == "" || originID == "" {
		return HostReplayResult{}, typed(ErrInvalidRecordCode, "project_id, delivery_run_id, origin_kind, and origin_id are required")
	}
	limit := boundHostReplayLimit(opts.Limit)
	leaseDuration := opts.LeaseDuration
	if leaseDuration <= 0 {
		leaseDuration = time.Minute
	}
	if leaseDuration > time.Hour {
		leaseDuration = time.Hour
	}

	cursor, err := latestReplayCursor(ctx, store, projectID, deliveryRunID, originKind, originID)
	if err != nil {
		return HostReplayResult{}, err
	}
	obligations, err := pendingHostReplayObligations(ctx, store, projectID, deliveryRunID, originID, cursor.ObligationID, now, limit)
	if err != nil {
		return HostReplayResult{}, err
	}
	result := HostReplayResult{OriginKind: originKind, OriginID: originID, DeliveryOpen: len(obligations) > 0}
	for _, obligation := range obligations {
		claim, err := ClaimDeliveryObligation(ctx, store, ClaimRequest{
			ObligationID:   obligation.ObligationID,
			ClaimOwner:     owner,
			LeaseExpiresAt: canonicalTimestamp(now.Add(leaseDuration)),
		})
		if err != nil {
			if errors.Is(err, ErrClaimConflict) || errors.Is(err, ErrStaleClaim) {
				continue
			}
			return result, err
		}
		receipt, order, err := loadReceiptByID(ctx, store, obligation.ProgressReceiptID)
		if err != nil {
			if releaseErr := releaseHostReplayClaim(ctx, store, claim.ClaimOwner, claim.ClaimGeneration, obligation.ObligationID); releaseErr != nil {
				err = errors.Join(err, releaseErr)
			}
			return result, err
		}
		view := ViewReceipt(receipt, order, "", now)
		view.DeliveryState = DeliveryStateView{
			State:     obligation.Status,
			Authority: "progress_delivery_obligations.status",
			Reason:    "transport_contract=" + displayValue(obligation.TransportContract),
		}
		if err := emit(view); err != nil {
			if releaseErr := releaseHostReplayClaim(ctx, store, claim.ClaimOwner, claim.ClaimGeneration, obligation.ObligationID); releaseErr != nil {
				err = errors.Join(err, releaseErr)
			}
			return result, err
		}
		cursorValue := prefixedDigest("preplay", map[string]any{
			"origin_kind":   originKind,
			"origin_id":     originID,
			"obligation_id": obligation.ObligationID,
		})
		if err := advanceDeliveryReplayCursorAndRelease(ctx, store, CursorAdvanceRequest{
			ObligationID: obligation.ObligationID,
			ClaimOwner:   claim.ClaimOwner,
			Generation:   claim.ClaimGeneration,
			OriginKind:   originKind,
			OriginID:     originID,
			CursorValue:  cursorValue,
			AdvancedAt:   canonicalTimestamp(now),
		}); err != nil {
			return result, err
		}
		result.Views = append(result.Views, view)
		result.Replayed++
		result.NextCursor = cursorValue
	}
	return result, nil
}

func boundHostReplayLimit(limit int) int {
	if limit <= 0 {
		return DefaultHostReplayLimit
	}
	if limit > MaxHostReplayLimit {
		return MaxHostReplayLimit
	}
	return limit
}

func latestReplayCursor(ctx context.Context, store storage.Store, projectID, deliveryRunID, originKind, originID string) (DeliveryReplayCursor, error) {
	cursors, err := ListDeliveryReplayCursors(ctx, store, DeliveryCursorFilter{
		ProjectID:     projectID,
		DeliveryRunID: deliveryRunID,
		OriginKind:    originKind,
		OriginID:      originID,
		Limit:         1,
	})
	if err != nil || len(cursors) == 0 {
		return DeliveryReplayCursor{}, err
	}
	return cursors[0], nil
}

func pendingHostReplayObligations(ctx context.Context, store storage.Store, projectID, deliveryRunID, sinkID, afterObligationID string, now time.Time, limit int) ([]DeliveryObligation, error) {
	afterCreatedAt, afterID, err := hostReplayCursorPosition(ctx, store, sanitizeID(afterObligationID))
	if err != nil {
		return nil, err
	}
	query := `SELECT obligation_id FROM progress_delivery_obligations
		WHERE project_id = ? AND delivery_run_id = ? AND sink_kind = 'host' AND sink_id = ?
			AND (status = ? OR (status = ? AND (next_attempt_at = '' OR next_attempt_at <= ?)))
			AND EXISTS (
				SELECT 1 FROM progress_receipts
				WHERE progress_receipts.progress_receipt_id = progress_delivery_obligations.progress_receipt_id
					AND (
						COALESCE(json_extract(progress_json, '$.state'), '') = 'terminal'
						OR phase = 'detached-terminal'
						OR status IN ('succeeded', 'failed', 'cancelled', 'needs-human', 'timed-out', 'abandoned')
						OR COALESCE(json_extract(blocker_json, '$.state'), '') NOT IN ('', 'none', 'unknown')
					)
			)
			AND (created_at > ? OR (created_at = ? AND obligation_id > ?))
		ORDER BY created_at, obligation_id LIMIT ?`
	args := []any{projectID, deliveryRunID, sinkID, DeliveryPending, DeliveryRetryableFailure, canonicalTimestamp(now), afterCreatedAt, afterCreatedAt, afterID, limit}
	var out []DeliveryObligation
	err = store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("list host replay obligations: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("list host replay obligations scan: %w", err)
			}
			obligation, err := loadDeliveryObligationTx(ctx, tx, id)
			if err != nil {
				return err
			}
			out = append(out, obligation)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("list host replay obligations rows: %w", err)
		}
		return nil
	})
	return out, err
}

func hostReplayCursorPosition(ctx context.Context, store storage.Store, obligationID string) (string, string, error) {
	if strings.TrimSpace(obligationID) == "" {
		return "", "", nil
	}
	var createdAt string
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT created_at FROM progress_delivery_obligations WHERE obligation_id = ?`, obligationID).Scan(&createdAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return typed(ErrMissingReferenceCode, "host replay cursor anchor %s not found", obligationID)
			}
			return fmt.Errorf("read host replay cursor position: %w", err)
		}
		return nil
	})
	if err != nil || createdAt == "" {
		return "", "", err
	}
	return createdAt, obligationID, nil
}

func releaseHostReplayClaim(ctx context.Context, store storage.Store, owner string, generation int64, obligationID string) error {
	if store == nil {
		return typed(ErrInvalidRecordCode, "store is required")
	}
	now := store.Now()
	owner = sanitizeID(owner)
	obligationID = sanitizeID(obligationID)
	if owner == "" || generation <= 0 || obligationID == "" {
		return typed(ErrInvalidRecordCode, "obligation and claim are required")
	}
	return store.WithWriteTx(ctx, func(tx storage.Tx) error {
		obligation, err := loadDeliveryObligationTx(ctx, tx, obligationID)
		if err != nil {
			return err
		}
		if err := requireCurrentClaim(obligation, owner, generation, now); err != nil {
			return err
		}
		res, err := tx.Exec(ctx, `UPDATE progress_delivery_obligations
			SET status = ?, claim_owner = '', claimed_at = '', claim_expires_at = '', updated_at = ?
			WHERE obligation_id = ? AND status = ? AND claim_owner = ? AND claim_generation = ?`,
			DeliveryPending, canonicalTimestamp(now), obligation.ObligationID, DeliveryAttempting, owner, generation)
		if err != nil {
			return fmt.Errorf("release host replay claim: %w", err)
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("release host replay claim rows affected: %w", err)
		}
		if rows != 1 {
			return typed(ErrStaleClaimCode, "obligation %s replay release lost active claim fence", obligation.ObligationID)
		}
		return nil
	})
}

func loadReceiptByID(ctx context.Context, store storage.Store, progressReceiptID string) (ProgressReceipt, int64, error) {
	var receipt ProgressReceipt
	var storageOrder int64
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		row := tx.QueryRow(ctx, `SELECT storage_order, payload_json FROM progress_receipts WHERE progress_receipt_id = ?`, sanitizeID(progressReceiptID))
		var payload string
		if err := row.Scan(&storageOrder, &payload); err != nil {
			if err == sql.ErrNoRows {
				return typed(ErrMissingReferenceCode, "progress receipt not found")
			}
			return fmt.Errorf("load host replay receipt: %w", err)
		}
		decoded, err := decodePersistedPayload([]byte(payload))
		if err != nil {
			return err
		}
		receipt = decoded
		return nil
	})
	return receipt, storageOrder, err
}

func advanceDeliveryReplayCursorAndRelease(ctx context.Context, store storage.Store, req CursorAdvanceRequest) error {
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
		if _, err := tx.Exec(ctx, `INSERT INTO progress_delivery_replay_cursors(
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
			obligation.ObligationID, req.ClaimOwner, req.Generation, req.CursorValue, req.AdvancedAt, string(payloadJSON)); err != nil {
			return fmt.Errorf("advance delivery replay cursor: %w", err)
		}
		res, err := tx.Exec(ctx, `UPDATE progress_delivery_obligations
			SET status = ?, claim_owner = '', claimed_at = '', claim_expires_at = '', updated_at = ?
			WHERE obligation_id = ? AND status = ? AND claim_owner = ? AND claim_generation = ?`,
			DeliveryPending, canonicalTimestamp(now), obligation.ObligationID, DeliveryAttempting, req.ClaimOwner, req.Generation)
		if err != nil {
			return fmt.Errorf("release host replay claim: %w", err)
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("release host replay claim rows affected: %w", err)
		}
		if rows != 1 {
			return typed(ErrStaleClaimCode, "obligation %s replay release lost active claim fence", obligation.ObligationID)
		}
		return nil
	})
}
