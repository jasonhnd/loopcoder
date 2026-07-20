package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// ChildExecutionRequestRecord stores the immutable provider-neutral launch
// request separately from its fenced claim generation and lifecycle state.
// RequestJSON is never rewritten after first acceptance.
type ChildExecutionRequestRecord struct {
	ChildRunID          string `json:"child_run_id"`
	ParentRunID         string `json:"parent_run_id"`
	PlanID              string `json:"plan_id"`
	ChildKey            string `json:"child_key"`
	SchemaVersion       string `json:"schema_version"`
	RequestJSON         string `json:"request_json"`
	ContractFingerprint string `json:"contract_fingerprint"`
	RepositoryIdentity  string `json:"repository_identity"`
	CheckoutIdentity    string `json:"checkout_identity"`
	Permission          string `json:"permission"`
	ScopeJSON           string `json:"scope_json"`
	ClaimGeneration     int64  `json:"claim_generation"`
	LifecycleStatus     string `json:"lifecycle_status"`
	LegacyAmbiguous     bool   `json:"legacy_ambiguous"`
	CreatedAt           string `json:"created_at"`
	UpdatedAt           string `json:"updated_at"`
}

// PersistChildPlanGraphWithExecutionRequests accepts a child graph and its
// immutable execution contracts atomically. Existing executable children must
// already have an identical non-legacy contract; missing or changed contracts
// fail closed. Migrated legacy contracts are replayable only while their run
// remains terminal or needs human resolution.
func PersistChildPlanGraphWithExecutionRequests(ctx context.Context, store Store, parent RunNode, children []RunNode, plan ChildPlanRecord, edges []RunEdgeRecord, requests []ChildExecutionRequestRecord) error {
	if store == nil {
		return nil
	}
	if strings.TrimSpace(plan.PlanID) == "" {
		return fmt.Errorf("persist child plan execution contracts: plan_id is required")
	}
	if len(children) != len(edges) || len(edges) != len(requests) {
		return fmt.Errorf("persist child plan execution contracts: child/edge/request count mismatch")
	}
	return withRetry(ctx, func() error {
		return persistChildPlanGraphWithExecutionRequestsOnce(ctx, store, parent, children, plan, edges, requests)
	})
}

func persistChildPlanGraphWithExecutionRequestsOnce(ctx context.Context, store Store, parent RunNode, children []RunNode, plan ChildPlanRecord, edges []RunEdgeRecord, requests []ChildExecutionRequestRecord) error {
	return store.WithWriteTx(ctx, func(tx Tx) error {
		if err := validateChildPlanGraph(ctx, tx, parent, children, plan, edges); err != nil {
			return err
		}
		existing := make([]bool, len(edges))
		for i := range requests {
			if err := validateChildExecutionRequestRecord(requests[i], plan, edges[i]); err != nil {
				return err
			}
			var childRunID string
			err := tx.QueryRow(ctx, `SELECT child_run_id FROM run_edges WHERE plan_id = ? AND child_key = ?`, plan.PlanID, edges[i].ChildKey).Scan(&childRunID)
			switch err {
			case nil:
				existing[i] = true
				if childRunID != requests[i].ChildRunID {
					return federationError(ErrDuplicateReplayCode, "child execution request replay changed run_id for plan %q child %q", plan.PlanID, edges[i].ChildKey)
				}
				stored, ok, err := loadChildExecutionRequestTx(ctx, tx, childRunID)
				if err != nil {
					return err
				}
				if !ok {
					return federationError(ErrInvalidRecordCode, "child execution request for plan %q child %q is missing; human review is required", plan.PlanID, edges[i].ChildKey)
				}
				if stored.LegacyAmbiguous {
					status, found, statusErr := currentRunStatus(ctx, tx, childRunID)
					if statusErr != nil {
						return statusErr
					}
					if found && (durableBlockedStatus(normalizeDurableStatus(status)) || durableTerminalStatus(normalizeDurableStatus(status))) {
						continue
					}
					return federationError(ErrInvalidRecordCode, "child execution request for plan %q child %q is legacy-ambiguous and executable; human review is required", plan.PlanID, edges[i].ChildKey)
				}
				if stored.ContractFingerprint != requests[i].ContractFingerprint || stored.RequestJSON != requests[i].RequestJSON {
					return federationError(ErrAgentFingerprintMismatchCode, "child execution request fingerprint mismatch for plan %q child %q", plan.PlanID, edges[i].ChildKey)
				}
			case sql.ErrNoRows:
			default:
				return fmt.Errorf("inspect child execution request replay %s/%s: %w", plan.PlanID, edges[i].ChildKey, err)
			}
		}
		if err := upsertRunNode(ctx, tx, parent); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO child_plans(plan_id, parent_run_id, root_run_id, schema_version, max_depth, max_concurrency, plan_json, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(plan_id) DO UPDATE SET
				parent_run_id = excluded.parent_run_id,
				root_run_id = excluded.root_run_id,
				schema_version = excluded.schema_version,
				max_depth = excluded.max_depth,
				max_concurrency = excluded.max_concurrency,
				plan_json = excluded.plan_json,
				created_at = excluded.created_at`,
			plan.PlanID, plan.ParentRunID, plan.RootRunID, plan.SchemaVersion, plan.MaxDepth, plan.MaxConcurrency, plan.PlanJSON, plan.CreatedAt); err != nil {
			return fmt.Errorf("persist child plan %s: %w", plan.PlanID, err)
		}
		for i, child := range children {
			if err := upsertRunNode(ctx, tx, child); err != nil {
				return err
			}
			if err := upsertRunEdge(ctx, tx, edges[i]); err != nil {
				return err
			}
			if existing[i] {
				continue
			}
			request := requests[i]
			if _, err := tx.Exec(ctx, `INSERT INTO child_execution_requests(
					child_run_id, parent_run_id, plan_id, child_key, schema_version,
					request_json, contract_fingerprint, repository_identity, checkout_identity,
					permission, scope_json, claim_generation, lifecycle_status,
					legacy_ambiguous, created_at, updated_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
				request.ChildRunID, request.ParentRunID, request.PlanID, request.ChildKey,
				request.SchemaVersion, request.RequestJSON, request.ContractFingerprint,
				request.RepositoryIdentity, request.CheckoutIdentity, request.Permission,
				request.ScopeJSON, request.ClaimGeneration, request.LifecycleStatus,
				request.CreatedAt, request.UpdatedAt); err != nil {
				return fmt.Errorf("persist child execution request %s: %w", request.ChildRunID, err)
			}
		}
		return nil
	})
}

// LoadChildExecutionRequest returns the durable request and its current fenced
// runtime binding. Missing records are reported with ok=false.
func LoadChildExecutionRequest(ctx context.Context, store Store, childRunID string) (ChildExecutionRequestRecord, bool, error) {
	if store == nil {
		return ChildExecutionRequestRecord{}, false, nil
	}
	childRunID = strings.TrimSpace(childRunID)
	if childRunID == "" {
		return ChildExecutionRequestRecord{}, false, fmt.Errorf("load child execution request: child_run_id is required")
	}
	var record ChildExecutionRequestRecord
	found := false
	err := store.WithTx(ctx, func(tx Tx) error {
		var err error
		record, found, err = loadChildExecutionRequestTx(ctx, tx, childRunID)
		return err
	})
	return record, found, err
}

// BindChildExecutionRequestClaim fences an immutable request to the current
// durable claim before any executor sees it.
func BindChildExecutionRequestClaim(ctx context.Context, store Store, childRunID, executorID string, claimGeneration int64, contractFingerprint, lifecycleStatus, updatedAt string) (ChildExecutionRequestRecord, error) {
	childRunID = strings.TrimSpace(childRunID)
	executorID = strings.TrimSpace(executorID)
	contractFingerprint = strings.TrimSpace(contractFingerprint)
	lifecycleStatus = strings.TrimSpace(lifecycleStatus)
	updatedAt = strings.TrimSpace(updatedAt)
	if childRunID == "" || executorID == "" || claimGeneration <= 0 || contractFingerprint == "" || lifecycleStatus == "" || updatedAt == "" {
		return ChildExecutionRequestRecord{}, fmt.Errorf("bind child execution request claim: child_run_id, executor_id, claim_generation, fingerprint, lifecycle_status, and updated_at are required")
	}
	if store == nil {
		return ChildExecutionRequestRecord{}, nil
	}
	var bound ChildExecutionRequestRecord
	err := withRetry(ctx, func() error {
		return store.WithWriteTx(ctx, func(tx Tx) error {
			var owner string
			var generation int64
			if err := tx.QueryRow(ctx, `SELECT executor_id, claim_generation FROM run_claims WHERE run_id = ?`, childRunID).Scan(&owner, &generation); err != nil {
				if err == sql.ErrNoRows {
					return staleChildRunClaimError(childRunID, claimGeneration)
				}
				return fmt.Errorf("bind child execution request claim: inspect claim: %w", err)
			}
			if owner != executorID || generation != claimGeneration {
				return staleChildRunClaimError(childRunID, claimGeneration)
			}
			record, ok, err := loadChildExecutionRequestTx(ctx, tx, childRunID)
			if err != nil {
				return err
			}
			if !ok || record.LegacyAmbiguous {
				return federationError(ErrInvalidRecordCode, "child execution request %q is missing or legacy-ambiguous; human review is required", childRunID)
			}
			if record.ContractFingerprint != contractFingerprint {
				return federationError(ErrAgentFingerprintMismatchCode, "child execution request fingerprint mismatch for run %q", childRunID)
			}
			if record.ClaimGeneration > claimGeneration {
				return staleChildRunClaimError(childRunID, claimGeneration)
			}
			if _, err := tx.Exec(ctx, `UPDATE child_execution_requests
				SET claim_generation = ?, lifecycle_status = ?, updated_at = ?
				WHERE child_run_id = ? AND contract_fingerprint = ? AND legacy_ambiguous = 0`,
				claimGeneration, lifecycleStatus, updatedAt, childRunID, contractFingerprint); err != nil {
				return fmt.Errorf("bind child execution request claim: %w", err)
			}
			record.ClaimGeneration = claimGeneration
			record.LifecycleStatus = lifecycleStatus
			record.UpdatedAt = updatedAt
			bound = record
			return nil
		})
	})
	return bound, err
}

// RejectClaimedChildExecutionRequest closes a fenced claim without launching
// a provider when its persisted execution contract is missing, ambiguous, or
// does not match the expected fingerprint.
func RejectClaimedChildExecutionRequest(ctx context.Context, store Store, parentRunID, childRunID, executorID string, claimGeneration int64, updatedAt, reason string) error {
	if store == nil {
		return nil
	}
	parentRunID = strings.TrimSpace(parentRunID)
	childRunID = strings.TrimSpace(childRunID)
	executorID = strings.TrimSpace(executorID)
	updatedAt = strings.TrimSpace(updatedAt)
	reason = strings.TrimSpace(reason)
	if parentRunID == "" || childRunID == "" || executorID == "" || claimGeneration <= 0 || updatedAt == "" {
		return fmt.Errorf("reject claimed child execution request: parent, child, executor, generation, and updated_at are required")
	}
	return withRetry(ctx, func() error {
		return store.WithWriteTx(ctx, func(tx Tx) error {
			result, err := tx.Exec(ctx, `UPDATE run_claims SET phase = ?, heartbeat_at = ? WHERE run_id = ? AND executor_id = ? AND claim_generation = ?`,
				ClaimPhaseCompleted, updatedAt, childRunID, executorID, claimGeneration)
			if err != nil {
				return fmt.Errorf("reject claimed child execution request: fence claim: %w", err)
			}
			if affected, rowsErr := result.RowsAffected(); rowsErr == nil && affected == 0 {
				return staleChildRunClaimError(childRunID, claimGeneration)
			}
			if _, err := tx.Exec(ctx, `UPDATE child_execution_requests
				SET claim_generation = ?, lifecycle_status = 'needs-human', updated_at = ?
				WHERE child_run_id = ?`, claimGeneration, updatedAt, childRunID); err != nil {
				return fmt.Errorf("reject claimed child execution request: mark contract: %w", err)
			}
			if err := transitionRunStatusTx(ctx, tx, RunStatusTransition{
				RunID:       childRunID,
				ParentRunID: parentRunID,
				ChildRunID:  childRunID,
				Status:      "needs-human",
				UpdatedAt:   updatedAt,
				Reason:      reason,
				Source:      "child-execution-contract",
			}); err != nil {
				return err
			}
			if err := releaseNestedSchedulerReservationsTx(ctx, tx, childRunID, executorID, claimGeneration, updatedAt); err != nil {
				return err
			}
			if err := releaseNestedSchedulerBudgetTx(ctx, tx, childRunID, executorID, claimGeneration, "needs-human", updatedAt); err != nil {
				return err
			}
			return completeNativeRegistrationForRunTx(ctx, tx, childRunID, executorID, claimGeneration, "needs-human", updatedAt, "")
		})
	})
}

func validateChildExecutionRequestRecord(request ChildExecutionRequestRecord, plan ChildPlanRecord, edge RunEdgeRecord) error {
	request.ChildRunID = strings.TrimSpace(request.ChildRunID)
	request.ParentRunID = strings.TrimSpace(request.ParentRunID)
	request.PlanID = strings.TrimSpace(request.PlanID)
	request.ChildKey = strings.TrimSpace(request.ChildKey)
	request.SchemaVersion = strings.TrimSpace(request.SchemaVersion)
	request.ContractFingerprint = strings.TrimSpace(request.ContractFingerprint)
	request.RepositoryIdentity = strings.TrimSpace(request.RepositoryIdentity)
	request.CheckoutIdentity = strings.TrimSpace(request.CheckoutIdentity)
	request.Permission = strings.TrimSpace(request.Permission)
	request.LifecycleStatus = strings.TrimSpace(request.LifecycleStatus)
	request.CreatedAt = strings.TrimSpace(request.CreatedAt)
	request.UpdatedAt = strings.TrimSpace(request.UpdatedAt)
	if request.ChildRunID == "" || request.ParentRunID == "" || request.PlanID == "" || request.ChildKey == "" || request.SchemaVersion == "" || request.ContractFingerprint == "" {
		return fmt.Errorf("persist child execution request: run, parent, plan, child, schema, and fingerprint are required")
	}
	if request.RepositoryIdentity == "" || request.CheckoutIdentity == "" || request.Permission == "" || request.LifecycleStatus == "" || request.CreatedAt == "" || request.UpdatedAt == "" {
		return fmt.Errorf("persist child execution request %s: repository, checkout, permission, lifecycle, and timestamps are required", request.ChildRunID)
	}
	if request.ClaimGeneration != 0 || request.LegacyAmbiguous {
		return fmt.Errorf("persist child execution request %s: a new contract must be unclaimed and non-legacy", request.ChildRunID)
	}
	if !json.Valid([]byte(request.RequestJSON)) || !json.Valid([]byte(request.ScopeJSON)) {
		return fmt.Errorf("persist child execution request %s: request_json and scope_json must be valid JSON", request.ChildRunID)
	}
	var requestScope, edgeScope any
	if err := json.Unmarshal([]byte(request.ScopeJSON), &requestScope); err != nil {
		return fmt.Errorf("persist child execution request %s: decode scope_json: %w", request.ChildRunID, err)
	}
	if err := json.Unmarshal([]byte(edge.ScopeJSON), &edgeScope); err != nil {
		return fmt.Errorf("persist child execution request %s: decode edge scope_json: %w", request.ChildRunID, err)
	}
	if !reflect.DeepEqual(requestScope, edgeScope) {
		return fmt.Errorf("persist child execution request %s: scope projection mismatch", request.ChildRunID)
	}
	if request.ChildRunID != edge.ChildRunID || request.ParentRunID != edge.ParentRunID || request.PlanID != plan.PlanID || request.PlanID != edge.PlanID || request.ChildKey != edge.ChildKey {
		return fmt.Errorf("persist child execution request %s: graph identity mismatch", request.ChildRunID)
	}
	if request.Permission != edge.Permission {
		return fmt.Errorf("persist child execution request %s: permission mismatch", request.ChildRunID)
	}
	return nil
}

func loadChildExecutionRequestTx(ctx context.Context, tx Tx, childRunID string) (ChildExecutionRequestRecord, bool, error) {
	var record ChildExecutionRequestRecord
	var legacy int
	err := tx.QueryRow(ctx, `SELECT
			child_run_id, parent_run_id, plan_id, child_key, schema_version,
			request_json, contract_fingerprint, repository_identity, checkout_identity,
			permission, scope_json, claim_generation, lifecycle_status,
			legacy_ambiguous, created_at, updated_at
		FROM child_execution_requests WHERE child_run_id = ?`, childRunID).Scan(
		&record.ChildRunID, &record.ParentRunID, &record.PlanID, &record.ChildKey,
		&record.SchemaVersion, &record.RequestJSON, &record.ContractFingerprint,
		&record.RepositoryIdentity, &record.CheckoutIdentity, &record.Permission,
		&record.ScopeJSON, &record.ClaimGeneration, &record.LifecycleStatus,
		&legacy, &record.CreatedAt, &record.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return ChildExecutionRequestRecord{}, false, nil
		}
		return ChildExecutionRequestRecord{}, false, fmt.Errorf("load child execution request %s: %w", childRunID, err)
	}
	record.LegacyAmbiguous = legacy != 0
	return record, true, nil
}

func updateChildExecutionRequestLifecycleTx(ctx context.Context, tx Tx, childRunID string, claimGeneration int64, lifecycleStatus, updatedAt string) error {
	record, ok, err := loadChildExecutionRequestTx(ctx, tx, childRunID)
	if err != nil || !ok {
		return err
	}
	if record.LegacyAmbiguous {
		return federationError(ErrInvalidRecordCode, "child execution request %q is legacy-ambiguous; human review is required", childRunID)
	}
	if record.ClaimGeneration != claimGeneration {
		return staleChildRunClaimError(childRunID, claimGeneration)
	}
	if _, err := tx.Exec(ctx, `UPDATE child_execution_requests SET lifecycle_status = ?, updated_at = ? WHERE child_run_id = ? AND claim_generation = ?`,
		lifecycleStatus, updatedAt, childRunID, claimGeneration); err != nil {
		return fmt.Errorf("update child execution request lifecycle: %w", err)
	}
	return nil
}
