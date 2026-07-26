package projectschema

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/authoritystore"
)

// Typed append results.
var (
	ErrIdempotencyConflict = errors.New("projectschema: idempotency key conflict")
	ErrBusy                = errors.New("projectschema: store busy")
	ErrNotCommitted        = errors.New("projectschema: append not committed")
)

// AppendRequest is one event append attempt.
type AppendRequest struct {
	EventID        string
	ProjectID      string
	AggregateKind  string
	AggregateID    string
	Kind           string
	IdempotencyKey string
	PayloadJSON    string
	CausalEventID  string
	EvidenceRefID  string
	// CheckpointName, when set, updates projection_checkpoints in the same txn.
	CheckpointName string
	RecordedAt     time.Time
}

// AppendResult is the durable evidence returned after a successful or reused append.
type AppendResult struct {
	EventID        string
	ProjectID      string
	Sequence       int64
	IdempotencyKey string
	Digest         string
	Reused         bool
	Retryable      bool
}

// Append inserts one event with monotonic project sequence and idempotent retry.
//
// Identical (project_id, idempotency_key, canonical digest) retries return the
// original event without allocating a new sequence. Same key with different
// digest is ErrIdempotencyConflict.
func Append(ctx context.Context, ps *authoritystore.ProjectStore, req AppendRequest) (AppendResult, error) {
	if ps == nil || ps.Foundation() == nil {
		return AppendResult{}, fmt.Errorf("projectschema: nil project store")
	}
	if err := validateAppendRequest(req); err != nil {
		return AppendResult{}, err
	}
	if req.RecordedAt.IsZero() {
		req.RecordedAt = time.Now().UTC()
	}
	digest, err := CanonicalDigest(req)
	if err != nil {
		return AppendResult{}, err
	}

	var result AppendResult
	err = ps.Foundation().WithDB(func(db *sql.DB) error {
		if err := ensure(ctx, db); err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return mapBusy(err)
		}
		defer tx.Rollback()

		// Existing idempotency key?
		var existingID string
		var existingSeq int64
		var existingPayload string
		var existingKind string
		var existingAggKind string
		var existingAggID string
		err = tx.QueryRowContext(ctx, `SELECT event_id, sequence, payload_json, kind, aggregate_kind, aggregate_id
			FROM events WHERE project_id=? AND idempotency_key=?`,
			req.ProjectID, req.IdempotencyKey,
		).Scan(&existingID, &existingSeq, &existingPayload, &existingKind, &existingAggKind, &existingAggID)
		if err == nil {
			// Rebuild digest of existing
			existReq := req
			existReq.EventID = existingID
			existReq.PayloadJSON = existingPayload
			existReq.Kind = existingKind
			existReq.AggregateKind = existingAggKind
			existReq.AggregateID = existingAggID
			existDigest, dErr := CanonicalDigest(existReq)
			if dErr != nil {
				return dErr
			}
			// Compare content-bearing fields for conflict
			if !sameIdempotentContent(req, existingKind, existingAggKind, existingAggID, existingPayload, digest, existDigest) {
				return ErrIdempotencyConflict
			}
			result = AppendResult{
				EventID: existingID, ProjectID: req.ProjectID, Sequence: existingSeq,
				IdempotencyKey: req.IdempotencyKey, Digest: existDigest, Reused: true,
			}
			return nil // no commit needed; read-only reuse
		}
		if err != nil && err != sql.ErrNoRows {
			return mapBusy(err)
		}

		// Allocate next sequence
		var maxSeq sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(sequence) FROM events WHERE project_id=?`, req.ProjectID).Scan(&maxSeq); err != nil {
			return mapBusy(err)
		}
		next := int64(1)
		if maxSeq.Valid {
			next = maxSeq.Int64 + 1
		}

		eventID := strings.TrimSpace(req.EventID)
		if eventID == "" {
			eventID = fmt.Sprintf("evt_%s_%d", shortHex(digest), next)
		}
		ts := req.RecordedAt.UTC().Format(time.RFC3339Nano)
		_, err = tx.ExecContext(ctx, `INSERT INTO events(
			event_id, project_id, aggregate_kind, aggregate_id, kind, envelope_version,
			sequence, recorded_at, idempotency_key, payload_version, payload_json,
			causal_event_id, evidence_ref_id
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			eventID, req.ProjectID, req.AggregateKind, req.AggregateID, req.Kind, EventEnvelopeVersion,
			next, ts, req.IdempotencyKey, 1, req.PayloadJSON,
			req.CausalEventID, req.EvidenceRefID,
		)
		if err != nil {
			if isUnique(err) {
				// Race: concurrent identical insert — re-read
				return fmt.Errorf("%w: concurrent idempotency race", ErrBusy)
			}
			return mapBusy(err)
		}

		if name := strings.TrimSpace(req.CheckpointName); name != "" {
			_, err = tx.ExecContext(ctx, `INSERT INTO projection_checkpoints(project_id, projection_name, last_sequence, updated_at)
				VALUES(?,?,?,?)
				ON CONFLICT(project_id, projection_name) DO UPDATE SET last_sequence=excluded.last_sequence, updated_at=excluded.updated_at`,
				req.ProjectID, name, next, ts,
			)
			if err != nil {
				return mapBusy(err)
			}
		}

		if err := tx.Commit(); err != nil {
			return mapBusy(err)
		}
		result = AppendResult{
			EventID: eventID, ProjectID: req.ProjectID, Sequence: next,
			IdempotencyKey: req.IdempotencyKey, Digest: digest, Reused: false,
		}
		return nil
	})
	if err != nil {
		retryable := errors.Is(err, ErrBusy)
		return AppendResult{Retryable: retryable}, err
	}
	return result, nil
}

func validateAppendRequest(req AppendRequest) error {
	if strings.TrimSpace(req.ProjectID) == "" {
		return fmt.Errorf("projectschema: project_id required")
	}
	if strings.TrimSpace(req.AggregateKind) == "" || strings.TrimSpace(req.AggregateID) == "" {
		return fmt.Errorf("projectschema: aggregate identity required")
	}
	if strings.TrimSpace(req.Kind) == "" {
		return fmt.Errorf("projectschema: kind required")
	}
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return fmt.Errorf("projectschema: idempotency_key required")
	}
	if req.PayloadJSON == "" {
		req.PayloadJSON = "{}"
	}
	return ValidatePayloadBound(req.PayloadJSON)
}

// CanonicalDigest hashes the content-bearing append fields (not event_id/sequence).
func CanonicalDigest(req AppendRequest) (string, error) {
	payload := req.PayloadJSON
	if payload == "" {
		payload = "{}"
	}
	// Normalize JSON if possible
	var raw any
	if err := json.Unmarshal([]byte(payload), &raw); err == nil {
		b, err := json.Marshal(raw)
		if err == nil {
			payload = string(b)
		}
	}
	doc := struct {
		ProjectID      string `json:"project_id"`
		AggregateKind  string `json:"aggregate_kind"`
		AggregateID    string `json:"aggregate_id"`
		Kind           string `json:"kind"`
		IdempotencyKey string `json:"idempotency_key"`
		Payload        string `json:"payload_json"`
		Causal         string `json:"causal_event_id"`
		Evidence       string `json:"evidence_ref_id"`
	}{
		ProjectID: req.ProjectID, AggregateKind: req.AggregateKind, AggregateID: req.AggregateID,
		Kind: req.Kind, IdempotencyKey: req.IdempotencyKey, Payload: payload,
		Causal: req.CausalEventID, Evidence: req.EvidenceRefID,
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func sameIdempotentContent(req AppendRequest, kind, aggKind, aggID, payload, newDigest, existDigest string) bool {
	if newDigest == existDigest {
		return true
	}
	// Fallback field compare
	return req.Kind == kind && req.AggregateKind == aggKind && req.AggregateID == aggID &&
		normalizeJSON(req.PayloadJSON) == normalizeJSON(payload)
}

func normalizeJSON(s string) string {
	var raw any
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return s
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return s
	}
	return string(b)
}

func mapBusy(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "busy") || strings.Contains(msg, "locked") {
		return fmt.Errorf("%w: %v", ErrBusy, err)
	}
	return err
}

func isUnique(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "constraint")
}

func shortHex(digest string) string {
	digest = strings.TrimPrefix(digest, "sha256:")
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}
