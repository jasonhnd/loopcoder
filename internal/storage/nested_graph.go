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
	ClaimOutcomeClaimed        = "claimed"
	ClaimOutcomeAlreadyRunning = "already-running"
	ClaimOutcomeTerminalReused = "terminal-reused"
	ClaimOutcomeBlocked        = "blocked"
	ClaimOutcomeStaleClaim     = "stale-claim"

	ClaimPhaseClaimed   = "claimed"
	ClaimPhaseLaunching = "launching"
	ClaimPhaseExecuting = "executing"
	ClaimPhaseCompleted = "completed"
)

var ErrStaleChildRunClaim = errors.New("stale claim")
var ErrNestedSchedulerResourceExhausted = errors.New("nested scheduler resource exhausted")

const schedulerReservationStateActive = "active"
const schedulerReservationStateReleased = "released"

type SchedulerResourceReservationRequest struct {
	RootRunID                string
	ProviderKey              string
	RootMaxConcurrency       int
	ParentMaxConcurrency     int
	ProviderMaxConcurrency   int
	ProjectID                string
	DeliveryRunID            string
	TaskID                   string
	SubAgentID               string
	AdapterID                string
	ProviderInstallationID   string
	AccountProfileID         string
	ModelCapabilityID        string
	RoutingDecisionID        string
	RoutingFingerprint       string
	PlanFingerprint          string
	PolicyFingerprint        string
	AuthorizationFingerprint string
	BudgetRequestedValue     int64
	BudgetQuantityKind       string
	BudgetUnit               string
	BudgetValueScale         int
	BudgetWindowKind         string
	AttachBudgetBinding      bool
	RequireBudgetAuthority   bool
}

// RunNode describes the durable run graph metadata required for nested runs.
type RunNode struct {
	RunID       string
	ProjectID   string
	ParentRunID string
	RootRunID   string
	Depth       int
	Origin      string
	Status      string
	CreatedAt   string
	UpdatedAt   string
}

// ChildPlanRecord is the accepted child-plan envelope persisted before any
// child run is launched.
type ChildPlanRecord struct {
	PlanID         string
	ParentRunID    string
	RootRunID      string
	SchemaVersion  string
	MaxDepth       int
	MaxConcurrency int
	PlanJSON       string
	CreatedAt      string
}

// RunEdgeRecord describes one parent-child relationship created from a plan.
type RunEdgeRecord struct {
	ParentRunID     string
	ChildRunID      string
	RootRunID       string
	PlanID          string
	ChildKey        string
	Depth           int
	Ordinal         int
	ScopeJSON       string
	Permission      string
	AggregationJSON string
	Status          string
	CreatedAt       string
	UpdatedAt       string
}

// ChildPlanReplayRecord is the durable state for a previously accepted child
// plan, keyed by plan_id and child_key.
type ChildPlanReplayRecord struct {
	Plan     ChildPlanRecord
	Children []ChildPlanReplayChild
}

// ChildPlanReplayChild is the persisted identity and recovery contract for one
// child in an accepted plan.
type ChildPlanReplayChild struct {
	ParentRunID     string
	ChildRunID      string
	RootRunID       string
	PlanID          string
	ChildKey        string
	Depth           int
	Ordinal         int
	ScopeJSON       string
	Permission      string
	AggregationJSON string
	EdgeStatus      string
	RunStatus       string
	StartedAt       string
	FinishedAt      string
	UpdatedAt       string
	ExecutorID      string
	ClaimGeneration int64
	ClaimedAt       string
	LeaseExpiresAt  string
	HeartbeatAt     string
	ClaimPhase      string
	ProviderKey     string
	ProviderReceipt string
}

// RunStatusTransition describes one authoritative durable status transition.
// When ParentRunID and ChildRunID are set, the child run and its parent edge are
// transitioned in the same transaction.
type RunStatusTransition struct {
	RunID       string
	ParentRunID string
	ChildRunID  string
	Status      string
	UpdatedAt   string
	Reason      string
	Source      string
}

type ClaimResult struct {
	Outcome         string
	RunID           string
	ExecutorID      string
	ClaimGeneration int64
	ClaimedAt       string
	LeaseExpiresAt  string
	HeartbeatAt     string
	RunStatus       string
	EdgeStatus      string
	ClaimPhase      string
	ProviderKey     string
	ProviderReceipt string
	PreviousOwner   string
	PreviousLease   string
}

func IsStaleChildRunClaim(err error) bool {
	return errors.Is(err, ErrStaleChildRunClaim)
}

func IsNestedSchedulerResourceExhausted(err error) bool {
	return errors.Is(err, ErrNestedSchedulerResourceExhausted)
}

func staleChildRunClaimError(runID string, claimGeneration int64) error {
	return fmt.Errorf("%w: %w for run %q generation %d", federationError(ErrStaleClaimCode, "current owner/generation does not match"), ErrStaleChildRunClaim, runID, claimGeneration)
}

// LoadChildPlanReplayRecord loads the authoritative durable child identity for
// a plan_id. Missing records are reported with ok=false.
func LoadChildPlanReplayRecord(ctx context.Context, store Store, planID string) (ChildPlanReplayRecord, bool, error) {
	if store == nil {
		return ChildPlanReplayRecord{}, false, nil
	}
	planID = strings.TrimSpace(planID)
	if planID == "" {
		return ChildPlanReplayRecord{}, false, fmt.Errorf("load child plan replay: plan_id is required")
	}
	var record ChildPlanReplayRecord
	found := false
	err := store.WithTx(ctx, func(tx Tx) error {
		err := tx.QueryRow(ctx, `SELECT plan_id, parent_run_id, root_run_id, schema_version, max_depth, max_concurrency, plan_json, created_at
			FROM child_plans WHERE plan_id = ?`, planID).Scan(
			&record.Plan.PlanID,
			&record.Plan.ParentRunID,
			&record.Plan.RootRunID,
			&record.Plan.SchemaVersion,
			&record.Plan.MaxDepth,
			&record.Plan.MaxConcurrency,
			&record.Plan.PlanJSON,
			&record.Plan.CreatedAt,
		)
		if err != nil {
			if err == sql.ErrNoRows {
				return nil
			}
			return fmt.Errorf("load child plan %s: %w", planID, err)
		}
		found = true
		rows, err := tx.Query(ctx, `SELECT
				e.parent_run_id,
				e.child_run_id,
				e.root_run_id,
				e.plan_id,
				e.child_key,
				e.depth,
				e.ordinal,
				e.scope_json,
				e.permission,
				e.aggregation_json,
				e.status,
				COALESCE(r.status, ''),
				COALESCE(r.started_at, ''),
				COALESCE(r.ended_at, ''),
				e.updated_at,
				COALESCE(c.executor_id, ''),
				COALESCE(c.claim_generation, 0),
				COALESCE(c.claimed_at, ''),
				COALESCE(c.lease_expires_at, ''),
				COALESCE(c.heartbeat_at, ''),
				COALESCE(c.phase, ''),
				COALESCE(c.provider_idempotency_key, ''),
				COALESCE(c.provider_receipt, '')
			FROM run_edges e
			LEFT JOIN runs r ON r.id = e.child_run_id
			LEFT JOIN run_claims c ON c.run_id = e.child_run_id
			WHERE e.plan_id = ?
			ORDER BY e.ordinal, e.child_key`, planID)
		if err != nil {
			return fmt.Errorf("load child plan %s edges: %w", planID, err)
		}
		defer rows.Close()
		for rows.Next() {
			var child ChildPlanReplayChild
			if err := rows.Scan(
				&child.ParentRunID,
				&child.ChildRunID,
				&child.RootRunID,
				&child.PlanID,
				&child.ChildKey,
				&child.Depth,
				&child.Ordinal,
				&child.ScopeJSON,
				&child.Permission,
				&child.AggregationJSON,
				&child.EdgeStatus,
				&child.RunStatus,
				&child.StartedAt,
				&child.FinishedAt,
				&child.UpdatedAt,
				&child.ExecutorID,
				&child.ClaimGeneration,
				&child.ClaimedAt,
				&child.LeaseExpiresAt,
				&child.HeartbeatAt,
				&child.ClaimPhase,
				&child.ProviderKey,
				&child.ProviderReceipt,
			); err != nil {
				return fmt.Errorf("load child plan %s edge: %w", planID, err)
			}
			record.Children = append(record.Children, child)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("load child plan %s edges: %w", planID, err)
		}
		return nil
	})
	if err != nil {
		return ChildPlanReplayRecord{}, false, err
	}
	return record, found, nil
}

// TransitionRunStatus validates and records one durable run transition and its
// run_events history entry transactionally.
func TransitionRunStatus(ctx context.Context, store Store, transition RunStatusTransition) error {
	if store == nil {
		return nil
	}
	transition.RunID = strings.TrimSpace(transition.RunID)
	transition.ParentRunID = strings.TrimSpace(transition.ParentRunID)
	transition.ChildRunID = strings.TrimSpace(transition.ChildRunID)
	transition.Status = normalizeDurableStatus(transition.Status)
	transition.UpdatedAt = strings.TrimSpace(transition.UpdatedAt)
	transition.Reason = strings.TrimSpace(transition.Reason)
	transition.Source = strings.TrimSpace(transition.Source)
	if transition.Source == "" {
		transition.Source = "storage"
	}
	if transition.RunID == "" {
		transition.RunID = transition.ChildRunID
	}
	if transition.RunID == "" || transition.Status == "" || transition.UpdatedAt == "" {
		return fmt.Errorf("transition run status: run_id, status, and updated_at are required")
	}
	if !validDurableStatus(transition.Status) {
		return fmt.Errorf("transition run status: invalid status %q", transition.Status)
	}
	if transition.ChildRunID != "" && transition.RunID != transition.ChildRunID {
		return fmt.Errorf("transition run status: run_id %q does not match child_run_id %q", transition.RunID, transition.ChildRunID)
	}
	if transition.ChildRunID != "" && transition.ParentRunID == "" {
		return fmt.Errorf("transition child run status: parent_run_id is required")
	}

	return withRetry(ctx, func() error {
		return store.WithWriteTx(ctx, func(tx Tx) error {
			return transitionRunStatusTx(ctx, tx, transition)
		})
	})
}

// TransitionChildRunStatus transitions a child run and its edge together.
func TransitionChildRunStatus(ctx context.Context, store Store, parentRunID, childRunID, status, updatedAt, reason string) error {
	return TransitionRunStatus(ctx, store, RunStatusTransition{
		RunID:       childRunID,
		ParentRunID: parentRunID,
		ChildRunID:  childRunID,
		Status:      status,
		UpdatedAt:   updatedAt,
		Reason:      reason,
		Source:      "nested-scheduler",
	})
}

// TransitionParentRunStatus transitions a parent run without changing edges.
func TransitionParentRunStatus(ctx context.Context, store Store, parentRunID, status, updatedAt, reason string) error {
	return TransitionRunStatus(ctx, store, RunStatusTransition{
		RunID:     parentRunID,
		Status:    status,
		UpdatedAt: updatedAt,
		Reason:    reason,
		Source:    "nested-scheduler",
	})
}

// PersistChildPlanGraph upserts the accepted plan, its child run nodes, and its
// plan edges in one transaction. Replaying the same plan_id/child_key pair is
// idempotent and keeps the original child_run_id.
func PersistChildPlanGraph(ctx context.Context, store Store, parent RunNode, children []RunNode, plan ChildPlanRecord, edges []RunEdgeRecord) error {
	if store == nil {
		return nil
	}
	if strings.TrimSpace(plan.PlanID) == "" {
		return fmt.Errorf("persist child plan graph: plan_id is required")
	}
	if len(children) != len(edges) {
		return fmt.Errorf("persist child plan graph: child/edge count mismatch")
	}
	return withRetry(ctx, func() error {
		return persistChildPlanGraphOnce(ctx, store, parent, children, plan, edges)
	})
}

func persistChildPlanGraphOnce(ctx context.Context, store Store, parent RunNode, children []RunNode, plan ChildPlanRecord, edges []RunEdgeRecord) error {
	return store.WithWriteTx(ctx, func(tx Tx) error {
		if err := validateChildPlanGraph(ctx, tx, parent, children, plan, edges); err != nil {
			return err
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
		}
		return nil
	})
}

func ClaimChildRunExecution(ctx context.Context, store Store, parentRunID, childRunID, executorID string, now, leaseUntil time.Time) (ClaimResult, error) {
	if store == nil {
		return ClaimResult{Outcome: ClaimOutcomeClaimed, RunID: strings.TrimSpace(childRunID), ExecutorID: strings.TrimSpace(executorID), ClaimGeneration: 1}, nil
	}
	parentRunID = strings.TrimSpace(parentRunID)
	childRunID = strings.TrimSpace(childRunID)
	executorID = strings.TrimSpace(executorID)
	if parentRunID == "" || childRunID == "" || executorID == "" {
		return ClaimResult{}, fmt.Errorf("claim child run execution: parent_run_id, child_run_id, and executor_id are required")
	}
	now = now.UTC()
	leaseUntil = leaseUntil.UTC()
	if now.IsZero() || leaseUntil.IsZero() || !leaseUntil.After(now) {
		return ClaimResult{}, fmt.Errorf("claim child run execution: valid now and future lease_until are required")
	}
	var result ClaimResult
	err := withRetry(ctx, func() error {
		var txResult ClaimResult
		err := store.WithWriteTx(ctx, func(tx Tx) error {
			claim, err := claimChildRunExecutionTx(ctx, tx, parentRunID, childRunID, executorID, formatTimestamp(now), formatTimestamp(leaseUntil))
			if err != nil {
				return err
			}
			txResult = claim
			return nil
		})
		if err != nil {
			return err
		}
		result = txResult
		return nil
	})
	return result, err
}

func ClaimChildRunExecutionWithReservations(ctx context.Context, store Store, parentRunID, childRunID, executorID string, now, leaseUntil time.Time, reservation SchedulerResourceReservationRequest) (ClaimResult, error) {
	if store == nil {
		return ClaimChildRunExecution(ctx, store, parentRunID, childRunID, executorID, now, leaseUntil)
	}
	parentRunID = strings.TrimSpace(parentRunID)
	childRunID = strings.TrimSpace(childRunID)
	executorID = strings.TrimSpace(executorID)
	if parentRunID == "" || childRunID == "" || executorID == "" {
		return ClaimResult{}, fmt.Errorf("claim child run execution: parent_run_id, child_run_id, and executor_id are required")
	}
	now = now.UTC()
	leaseUntil = leaseUntil.UTC()
	if now.IsZero() || leaseUntil.IsZero() || !leaseUntil.After(now) {
		return ClaimResult{}, fmt.Errorf("claim child run execution: valid now and future lease_until are required")
	}
	var result ClaimResult
	err := withRetry(ctx, func() error {
		var txResult ClaimResult
		err := store.WithWriteTx(ctx, func(tx Tx) error {
			claim, err := claimChildRunExecutionTx(ctx, tx, parentRunID, childRunID, executorID, formatTimestamp(now), formatTimestamp(leaseUntil))
			if err != nil {
				return err
			}
			switch claim.Outcome {
			case ClaimOutcomeClaimed, ClaimOutcomeStaleClaim:
				if err := reserveNestedSchedulerResourcesTx(ctx, tx, claim, parentRunID, reservation, formatTimestamp(now), formatTimestamp(leaseUntil)); err != nil {
					return err
				}
				if _, err := reserveNestedSchedulerBudgetTx(ctx, tx, claim, reservation, formatTimestamp(now), formatTimestamp(leaseUntil)); err != nil {
					return err
				}
			}
			txResult = claim
			return nil
		})
		if err != nil {
			return err
		}
		result = txResult
		return nil
	})
	return result, err
}

func RenewChildRunClaim(ctx context.Context, store Store, childRunID, executorID string, claimGeneration int64, now, leaseUntil time.Time) error {
	if store == nil {
		return nil
	}
	childRunID = strings.TrimSpace(childRunID)
	executorID = strings.TrimSpace(executorID)
	if childRunID == "" || executorID == "" || claimGeneration <= 0 {
		return fmt.Errorf("renew child run claim: child_run_id, executor_id, and claim_generation are required")
	}
	now = now.UTC()
	leaseUntil = leaseUntil.UTC()
	if now.IsZero() || leaseUntil.IsZero() || !leaseUntil.After(now) {
		return fmt.Errorf("renew child run claim: valid now and future lease_until are required")
	}
	return withRetry(ctx, func() error {
		return store.WithWriteTx(ctx, func(tx Tx) error {
			result, err := tx.Exec(ctx, `UPDATE run_claims
				SET heartbeat_at = ?, lease_expires_at = ?
				WHERE run_id = ? AND executor_id = ? AND claim_generation = ?`,
				formatTimestamp(now), formatTimestamp(leaseUntil), childRunID, executorID, claimGeneration)
			if err != nil {
				return fmt.Errorf("renew child run claim: %w", err)
			}
			affected, err := result.RowsAffected()
			if err == nil && affected == 0 {
				return staleChildRunClaimError(childRunID, claimGeneration)
			}
			if err := renewClaimFencedOwnershipLocksTx(ctx, tx, childRunID, executorID, claimGeneration, formatTimestamp(now), formatTimestamp(leaseUntil)); err != nil {
				return err
			}
			if err := renewNestedSchedulerReservationsTx(ctx, tx, childRunID, executorID, claimGeneration, formatTimestamp(now), formatTimestamp(leaseUntil)); err != nil {
				return err
			}
			if err := renewNestedSchedulerBudgetTx(ctx, tx, childRunID, executorID, claimGeneration, formatTimestamp(now), formatTimestamp(leaseUntil)); err != nil {
				return err
			}
			return nil
		})
	})
}

func UpdateChildRunClaimPhase(ctx context.Context, store Store, parentRunID, childRunID, executorID string, claimGeneration int64, phase, at, providerReceipt string) error {
	if store == nil {
		return nil
	}
	parentRunID = strings.TrimSpace(parentRunID)
	childRunID = strings.TrimSpace(childRunID)
	executorID = strings.TrimSpace(executorID)
	phase = normalizeClaimPhase(phase)
	at = strings.TrimSpace(at)
	providerReceipt = strings.TrimSpace(providerReceipt)
	if parentRunID == "" || childRunID == "" || executorID == "" || claimGeneration <= 0 || phase == "" || at == "" {
		return fmt.Errorf("update child run claim phase: parent_run_id, child_run_id, executor_id, claim_generation, phase, and at are required")
	}
	return withRetry(ctx, func() error {
		return store.WithWriteTx(ctx, func(tx Tx) error {
			result, err := tx.Exec(ctx, `UPDATE run_claims
				SET phase = ?, heartbeat_at = ?, provider_receipt = CASE WHEN ? <> '' THEN ? ELSE provider_receipt END
				WHERE run_id = ? AND executor_id = ? AND claim_generation = ?`,
				phase, at, providerReceipt, providerReceipt, childRunID, executorID, claimGeneration)
			if err != nil {
				return fmt.Errorf("update child run claim phase: %w", err)
			}
			affected, err := result.RowsAffected()
			if err == nil && affected == 0 {
				return staleChildRunClaimError(childRunID, claimGeneration)
			}
			status := ""
			switch phase {
			case ClaimPhaseLaunching:
				status = "launching"
			case ClaimPhaseExecuting:
				status = "running"
			}
			if status != "" {
				if err := transitionRunStatusTx(ctx, tx, RunStatusTransition{
					RunID:       childRunID,
					ParentRunID: parentRunID,
					ChildRunID:  childRunID,
					Status:      status,
					UpdatedAt:   at,
					Reason:      "child claim phase " + phase,
					Source:      "nested-scheduler",
				}); err != nil {
					return err
				}
			} else if err := appendClaimPhaseEvent(ctx, tx, childRunID, at, phase, "nested-scheduler"); err != nil {
				return err
			}
			return nil
		})
	})
}

func CompleteClaimedChildRun(ctx context.Context, store Store, parentRunID, childRunID, executorID string, claimGeneration int64, status, updatedAt, reason, providerReceipt string) error {
	if store == nil {
		return nil
	}
	parentRunID = strings.TrimSpace(parentRunID)
	childRunID = strings.TrimSpace(childRunID)
	executorID = strings.TrimSpace(executorID)
	status = normalizeDurableStatus(status)
	updatedAt = strings.TrimSpace(updatedAt)
	reason = strings.TrimSpace(reason)
	providerReceipt = strings.TrimSpace(providerReceipt)
	if parentRunID == "" || childRunID == "" || executorID == "" || claimGeneration <= 0 || status == "" || updatedAt == "" {
		return fmt.Errorf("complete claimed child run: parent_run_id, child_run_id, executor_id, claim_generation, status, and updated_at are required")
	}
	if !validDurableStatus(status) {
		return fmt.Errorf("complete claimed child run: invalid status %q", status)
	}
	return withRetry(ctx, func() error {
		return store.WithWriteTx(ctx, func(tx Tx) error {
			result, err := tx.Exec(ctx, `UPDATE run_claims
				SET phase = ?, heartbeat_at = ?, provider_receipt = CASE WHEN provider_receipt = '' THEN ? ELSE provider_receipt END
				WHERE run_id = ? AND executor_id = ? AND claim_generation = ?`,
				ClaimPhaseCompleted, updatedAt, providerReceipt, childRunID, executorID, claimGeneration)
			if err != nil {
				return fmt.Errorf("complete claimed child run: fence claim: %w", err)
			}
			affected, err := result.RowsAffected()
			if err == nil && affected == 0 {
				return staleChildRunClaimError(childRunID, claimGeneration)
			}
			if err := transitionRunStatusTx(ctx, tx, RunStatusTransition{
				RunID:       childRunID,
				ParentRunID: parentRunID,
				ChildRunID:  childRunID,
				Status:      status,
				UpdatedAt:   updatedAt,
				Reason:      reason,
				Source:      "nested-scheduler",
			}); err != nil {
				return err
			}
			if err := releaseNestedSchedulerReservationsTx(ctx, tx, childRunID, executorID, claimGeneration, updatedAt); err != nil {
				return err
			}
			if err := releaseNestedSchedulerBudgetTx(ctx, tx, childRunID, executorID, claimGeneration, status, updatedAt); err != nil {
				return err
			}
			return completeNativeRegistrationForRunTx(ctx, tx, childRunID, executorID, claimGeneration, status, updatedAt, providerReceipt)
		})
	})
}

func claimChildRunExecutionTx(ctx context.Context, tx Tx, parentRunID, childRunID, executorID, now, leaseUntil string) (ClaimResult, error) {
	runStatus, ok, err := currentRunStatus(ctx, tx, childRunID)
	if err != nil {
		return ClaimResult{}, err
	}
	if !ok {
		return ClaimResult{}, fmt.Errorf("claim child run execution: run %q is missing", childRunID)
	}
	edgeStatus, ok, err := currentRunEdgeStatus(ctx, tx, parentRunID, childRunID)
	if err != nil {
		return ClaimResult{}, err
	}
	if !ok {
		return ClaimResult{}, fmt.Errorf("claim child run execution: edge %q/%q is missing", parentRunID, childRunID)
	}
	status := normalizeDurableStatus(firstNonEmptyNestedGraph(runStatus, edgeStatus))
	base := ClaimResult{RunID: childRunID, RunStatus: runStatus, EdgeStatus: edgeStatus}
	if durableBlockedStatus(status) {
		base.Outcome = ClaimOutcomeBlocked
		return base, nil
	}
	if durableTerminalStatus(status) {
		base.Outcome = ClaimOutcomeTerminalReused
		return base, nil
	}

	claim, hasClaim, err := currentRunClaim(ctx, tx, childRunID)
	if err != nil {
		return ClaimResult{}, err
	}
	if hasClaim && claimLeaseActive(claim.LeaseExpiresAt, now) {
		claim.Outcome = ClaimOutcomeAlreadyRunning
		claim.RunStatus = runStatus
		claim.EdgeStatus = edgeStatus
		return claim, nil
	}
	if hasClaim && !claimPhaseAutoTakeoverAllowed(claim.ClaimPhase) {
		if err := transitionRunStatusTx(ctx, tx, RunStatusTransition{
			RunID:       childRunID,
			ParentRunID: parentRunID,
			ChildRunID:  childRunID,
			Status:      "needs-human",
			UpdatedAt:   now,
			Reason:      fmt.Sprintf("expired child execution claim in %s phase requires human recovery", normalizeClaimPhase(claim.ClaimPhase)),
			Source:      "nested-scheduler",
		}); err != nil {
			return ClaimResult{}, err
		}
		if err := markAgentRegistrationNeedsHumanForRunTx(ctx, tx, childRunID, now, fmt.Sprintf("expired child execution claim in %s phase requires human recovery", normalizeClaimPhase(claim.ClaimPhase))); err != nil {
			return ClaimResult{}, err
		}
		claim.Outcome = ClaimOutcomeBlocked
		claim.RunStatus = "needs-human"
		claim.EdgeStatus = "needs-human"
		claim.PreviousOwner = claim.ExecutorID
		claim.PreviousLease = claim.LeaseExpiresAt
		return claim, nil
	}

	generation := int64(1)
	outcome := ClaimOutcomeClaimed
	previousOwner := ""
	previousLease := ""
	if hasClaim {
		generation = claim.ClaimGeneration + 1
		outcome = ClaimOutcomeStaleClaim
		previousOwner = claim.ExecutorID
		previousLease = claim.LeaseExpiresAt
	}
	providerKey := providerIdempotencyKey(childRunID)
	if _, err := tx.Exec(ctx, `INSERT INTO run_claims(run_id, executor_id, claim_generation, claimed_at, lease_expires_at, heartbeat_at, phase, provider_idempotency_key, provider_receipt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, '')
		ON CONFLICT(run_id) DO UPDATE SET
			executor_id = excluded.executor_id,
			claim_generation = excluded.claim_generation,
			claimed_at = excluded.claimed_at,
			lease_expires_at = excluded.lease_expires_at,
			heartbeat_at = excluded.heartbeat_at,
			phase = excluded.phase,
			provider_idempotency_key = excluded.provider_idempotency_key,
			provider_receipt = excluded.provider_receipt`,
		childRunID, executorID, generation, now, leaseUntil, now, ClaimPhaseClaimed, providerKey); err != nil {
		return ClaimResult{}, fmt.Errorf("claim child run execution: persist claim: %w", err)
	}
	targetStatus := "launching"
	if status == "running" {
		targetStatus = "running"
	}
	if err := transitionRunStatusTx(ctx, tx, RunStatusTransition{
		RunID:       childRunID,
		ParentRunID: parentRunID,
		ChildRunID:  childRunID,
		Status:      targetStatus,
		UpdatedAt:   now,
		Reason:      "child execution claimed",
		Source:      "nested-scheduler",
	}); err != nil {
		return ClaimResult{}, err
	}
	return ClaimResult{
		Outcome:         outcome,
		RunID:           childRunID,
		ExecutorID:      executorID,
		ClaimGeneration: generation,
		ClaimedAt:       now,
		LeaseExpiresAt:  leaseUntil,
		HeartbeatAt:     now,
		ClaimPhase:      ClaimPhaseClaimed,
		ProviderKey:     providerKey,
		RunStatus:       targetStatus,
		EdgeStatus:      targetStatus,
		PreviousOwner:   previousOwner,
		PreviousLease:   previousLease,
	}, nil
}

func transitionRunStatusTx(ctx context.Context, tx Tx, transition RunStatusTransition) error {
	previous, ok, err := currentRunStatus(ctx, tx, transition.RunID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("transition run status: run %q is missing", transition.RunID)
	}
	if err := validateDurableTransition(previous, transition.Status); err != nil {
		return fmt.Errorf("transition run %s: %w", transition.RunID, err)
	}

	var previousEdge string
	if transition.ChildRunID != "" {
		previousEdge, ok, err = currentRunEdgeStatus(ctx, tx, transition.ParentRunID, transition.ChildRunID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("transition child run status: edge %q/%q is missing", transition.ParentRunID, transition.ChildRunID)
		}
		if err := validateDurableTransition(previousEdge, transition.Status); err != nil {
			return fmt.Errorf("transition run edge %s/%s: %w", transition.ParentRunID, transition.ChildRunID, err)
		}
	}

	if _, err := tx.Exec(ctx, `UPDATE runs SET
			status = ?,
			started_at = CASE WHEN ? IN ('launching', 'running') AND (started_at IS NULL OR started_at = '') THEN ? ELSE started_at END,
			ended_at = CASE WHEN ? IN ('succeeded', 'succeeded_with_optional_failures', 'failed', 'cancelled', 'timed_out', 'abandoned', 'needs-human', 'skipped', 'hung', 'idle', 'blocked') THEN ? WHEN ? IN ('queued', 'launching', 'running', 'waiting', 'finishing') THEN NULL ELSE ended_at END,
			updated_at = ?
		WHERE id = ?`,
		transition.Status, transition.Status, transition.UpdatedAt, transition.Status, transition.UpdatedAt, transition.Status, transition.UpdatedAt, transition.RunID); err != nil {
		return fmt.Errorf("transition run status: %w", err)
	}
	if transition.ChildRunID != "" {
		if _, err := tx.Exec(ctx, `UPDATE run_edges SET status = ?, updated_at = ? WHERE parent_run_id = ? AND child_run_id = ?`,
			transition.Status, transition.UpdatedAt, transition.ParentRunID, transition.ChildRunID); err != nil {
			return fmt.Errorf("transition run edge status: %w", err)
		}
	}
	payload, err := json.Marshal(map[string]string{
		"run_id":               transition.RunID,
		"parent_run_id":        transition.ParentRunID,
		"child_run_id":         transition.ChildRunID,
		"previous_status":      previous,
		"status":               transition.Status,
		"previous_edge_status": previousEdge,
		"reason":               transition.Reason,
		"source":               transition.Source,
	})
	if err != nil {
		return fmt.Errorf("marshal run transition event: %w", err)
	}
	if err := appendRunTransitionEvent(ctx, tx, transition.RunID, transition.UpdatedAt, "run.status.transition", string(payload)); err != nil {
		return err
	}
	return nil
}

func validateChildPlanGraph(ctx context.Context, tx Tx, parent RunNode, children []RunNode, plan ChildPlanRecord, edges []RunEdgeRecord) error {
	parent.RunID = strings.TrimSpace(parent.RunID)
	parent.ParentRunID = strings.TrimSpace(parent.ParentRunID)
	parent.RootRunID = strings.TrimSpace(firstNonEmptyNestedGraph(parent.RootRunID, plan.RootRunID, parent.RunID))
	plan.ParentRunID = strings.TrimSpace(plan.ParentRunID)
	plan.RootRunID = strings.TrimSpace(plan.RootRunID)
	if parent.RunID == "" || plan.ParentRunID == "" || plan.RootRunID == "" {
		return fmt.Errorf("persist child plan graph: parent_run_id and root_run_id are required")
	}
	if parent.RunID != plan.ParentRunID {
		return fmt.Errorf("persist child plan graph: parent run %q does not match plan parent %q", parent.RunID, plan.ParentRunID)
	}
	if parent.RootRunID != plan.RootRunID {
		return fmt.Errorf("persist child plan graph: parent root %q does not match plan root %q", parent.RootRunID, plan.RootRunID)
	}
	if parent.Depth < 0 {
		return fmt.Errorf("persist child plan graph: parent depth must be non-negative")
	}
	if parent.Depth == 0 && parent.RootRunID != parent.RunID {
		return fmt.Errorf("persist child plan graph: root mismatch: root parent %q must use itself as root, got %q", parent.RunID, parent.RootRunID)
	}
	if parent.ParentRunID == parent.RunID {
		return fmt.Errorf("persist child plan graph: parent %q cannot be its own parent", parent.RunID)
	}

	existingParent, ok, err := lookupRunNode(ctx, tx, parent.RunID)
	if err != nil {
		return err
	}
	if ok {
		if err := validateExistingRunCompatible("parent", parent, existingParent); err != nil {
			return err
		}
	} else if parent.Depth > 0 {
		return fmt.Errorf("persist child plan graph: non-root parent %q is missing from durable graph", parent.RunID)
	}
	if parent.Depth > 0 {
		ancestors, err := runAncestors(ctx, tx, parent.RunID)
		if err != nil {
			return err
		}
		if len(ancestors) == 0 {
			return fmt.Errorf("persist child plan graph: parent %q has no durable ancestor path", parent.RunID)
		}
		if !stringSetContains(ancestors, parent.RootRunID) {
			return fmt.Errorf("persist child plan graph: parent %q is not under root %q", parent.RunID, parent.RootRunID)
		}
	}

	seenChildren := map[string]bool{}
	seenOrdinals := map[int]string{}
	for i, child := range children {
		if i >= len(edges) {
			return fmt.Errorf("persist child plan graph: missing edge for child index %d", i)
		}
		edge := edges[i]
		child.RunID = strings.TrimSpace(child.RunID)
		child.ParentRunID = strings.TrimSpace(child.ParentRunID)
		child.RootRunID = strings.TrimSpace(child.RootRunID)
		edge.ParentRunID = strings.TrimSpace(edge.ParentRunID)
		edge.ChildRunID = strings.TrimSpace(edge.ChildRunID)
		edge.RootRunID = strings.TrimSpace(edge.RootRunID)
		if child.RunID == "" || edge.ChildRunID == "" {
			return fmt.Errorf("persist child plan graph: child run_id is required")
		}
		if child.RunID != edge.ChildRunID {
			return fmt.Errorf("persist child plan graph: child node %q does not match edge child %q", child.RunID, edge.ChildRunID)
		}
		if edge.ParentRunID != parent.RunID || child.ParentRunID != parent.RunID {
			return fmt.Errorf("persist child plan graph: child %q parent mismatch", child.RunID)
		}
		if edge.RootRunID != parent.RootRunID || child.RootRunID != parent.RootRunID {
			return fmt.Errorf("persist child plan graph: child %q root mismatch", child.RunID)
		}
		if child.Depth != parent.Depth+1 || edge.Depth != child.Depth {
			return fmt.Errorf("persist child plan graph: child %q depth mismatch", child.RunID)
		}
		if child.RunID == parent.RunID {
			return fmt.Errorf("persist child plan graph: child %q cannot reuse parent run id", child.RunID)
		}
		if child.RunID == parent.RootRunID {
			return fmt.Errorf("persist child plan graph: child %q cannot reuse root run id", child.RunID)
		}
		if seenChildren[child.RunID] {
			return fmt.Errorf("persist child plan graph: duplicate child run id %q", child.RunID)
		}
		seenChildren[child.RunID] = true
		if edge.Ordinal >= 0 {
			if previous := seenOrdinals[edge.Ordinal]; previous != "" {
				return fmt.Errorf("persist child plan graph: duplicate ordinal %d for children %q and %q", edge.Ordinal, previous, child.RunID)
			}
			seenOrdinals[edge.Ordinal] = child.RunID
		}
		if ancestors, err := runAncestors(ctx, tx, parent.RunID); err != nil {
			return err
		} else if stringSetContains(ancestors, child.RunID) {
			return fmt.Errorf("persist child plan graph: child %q cannot reuse ancestor run id", child.RunID)
		}
		if descendants, err := runDescendants(ctx, tx, child.RunID); err != nil {
			return err
		} else if stringSetContains(descendants, parent.RunID) {
			return fmt.Errorf("persist child plan graph: edge %q -> %q would create a cycle", parent.RunID, child.RunID)
		}
		existingChild, ok, err := lookupRunNode(ctx, tx, child.RunID)
		if err != nil {
			return err
		}
		if ok {
			if err := validateExistingRunCompatible("child", child, existingChild); err != nil {
				return err
			}
		}
		existingEdge, ok, err := lookupRunEdge(ctx, tx, edge.ParentRunID, edge.ChildRunID)
		if err != nil {
			return err
		}
		if ok {
			if existingEdge.RootRunID != "" && existingEdge.RootRunID != edge.RootRunID {
				return fmt.Errorf("persist child plan graph: existing edge %q/%q root mismatch: %q != %q", edge.ParentRunID, edge.ChildRunID, existingEdge.RootRunID, edge.RootRunID)
			}
			if existingEdge.Depth >= 0 && edge.Depth >= 0 && existingEdge.Depth != edge.Depth {
				return fmt.Errorf("persist child plan graph: existing edge %q/%q depth mismatch: %d != %d", edge.ParentRunID, edge.ChildRunID, existingEdge.Depth, edge.Depth)
			}
		}
	}
	return nil
}

// UpdateChildRunOutcome records a child edge and child run terminal status in
// one transaction.
func UpdateChildRunOutcome(ctx context.Context, store Store, parentRunID, childRunID, status, updatedAt string) error {
	return TransitionChildRunStatus(ctx, store, parentRunID, childRunID, status, updatedAt, "child outcome")
}

func upsertRunNode(ctx context.Context, tx Tx, run RunNode) error {
	run.RunID = strings.TrimSpace(run.RunID)
	if run.RunID == "" {
		return fmt.Errorf("persist run node: run_id is required")
	}
	if strings.TrimSpace(run.RootRunID) == "" {
		run.RootRunID = run.RunID
	}
	if strings.TrimSpace(run.CreatedAt) == "" {
		run.CreatedAt = run.UpdatedAt
	}
	if strings.TrimSpace(run.UpdatedAt) == "" {
		run.UpdatedAt = run.CreatedAt
	}
	_, err := tx.Exec(ctx, `INSERT INTO runs(id, project_id, parent_run_id, status, started_at, updated_at, root_run_id, depth, origin, created_at)
		VALUES (?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
			project_id = COALESCE(NULLIF(excluded.project_id, ''), runs.project_id),
			parent_run_id = COALESCE(NULLIF(excluded.parent_run_id, ''), runs.parent_run_id),
			status = CASE
				WHEN runs.status IN ('succeeded', 'succeeded_with_optional_failures', 'failed', 'cancelled', 'timed_out', 'abandoned', 'needs-human', 'hung', 'skipped') THEN runs.status
				WHEN runs.status <> '' THEN runs.status
				WHEN excluded.status <> '' THEN excluded.status
				ELSE runs.status
			END,
			started_at = COALESCE(NULLIF(runs.started_at, ''), NULLIF(excluded.started_at, '')),
			updated_at = CASE WHEN excluded.updated_at <> '' THEN excluded.updated_at ELSE runs.updated_at END,
			root_run_id = CASE WHEN excluded.root_run_id <> '' THEN excluded.root_run_id ELSE runs.root_run_id END,
			depth = excluded.depth,
			origin = CASE WHEN excluded.origin <> '' THEN excluded.origin ELSE runs.origin END,
			created_at = COALESCE(NULLIF(runs.created_at, ''), NULLIF(excluded.created_at, ''))`,
		run.RunID, run.ProjectID, run.ParentRunID, run.Status, run.CreatedAt, run.UpdatedAt, run.RootRunID, run.Depth, run.Origin, run.CreatedAt)
	if err != nil {
		return fmt.Errorf("persist run node %s: %w", run.RunID, err)
	}
	return nil
}

func upsertRunEdge(ctx context.Context, tx Tx, edge RunEdgeRecord) error {
	if strings.TrimSpace(edge.ParentRunID) == "" || strings.TrimSpace(edge.ChildRunID) == "" {
		return fmt.Errorf("persist run edge: parent_run_id and child_run_id are required")
	}
	if strings.TrimSpace(edge.ParentRunID) == strings.TrimSpace(edge.ChildRunID) {
		return fmt.Errorf("persist run edge: parent_run_id and child_run_id cannot be equal")
	}
	if strings.TrimSpace(edge.ScopeJSON) == "" {
		edge.ScopeJSON = "{}"
	}
	if !json.Valid([]byte(edge.ScopeJSON)) {
		return fmt.Errorf("persist run edge %s/%s: scope_json is invalid", edge.ParentRunID, edge.ChildRunID)
	}
	if strings.TrimSpace(edge.AggregationJSON) == "" {
		edge.AggregationJSON = "{}"
	}
	if !json.Valid([]byte(edge.AggregationJSON)) {
		return fmt.Errorf("persist run edge %s/%s: aggregation_json is invalid", edge.ParentRunID, edge.ChildRunID)
	}
	_, err := tx.Exec(ctx, `INSERT INTO run_edges(parent_run_id, child_run_id, edge_type, created_at, root_run_id, plan_id, child_key, depth, ordinal, scope_json, permission, aggregation_json, status, updated_at)
		VALUES (?, ?, 'child', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(parent_run_id, child_run_id) DO UPDATE SET
			root_run_id = excluded.root_run_id,
			plan_id = excluded.plan_id,
			child_key = excluded.child_key,
			depth = excluded.depth,
			ordinal = excluded.ordinal,
			scope_json = excluded.scope_json,
			permission = excluded.permission,
			aggregation_json = excluded.aggregation_json,
			status = CASE
				WHEN run_edges.status IN ('succeeded', 'succeeded_with_optional_failures', 'failed', 'cancelled', 'timed_out', 'abandoned', 'needs-human', 'hung', 'skipped') THEN run_edges.status
				WHEN run_edges.status <> '' THEN run_edges.status
				ELSE excluded.status
			END,
			updated_at = excluded.updated_at`,
		edge.ParentRunID, edge.ChildRunID, edge.CreatedAt, edge.RootRunID, edge.PlanID, edge.ChildKey, edge.Depth, edge.Ordinal, edge.ScopeJSON, edge.Permission, edge.AggregationJSON, edge.Status, edge.UpdatedAt)
	if err != nil {
		return fmt.Errorf("persist run edge %s/%s: %w", edge.ParentRunID, edge.ChildRunID, err)
	}
	return nil
}

type storedRunNode struct {
	RunID       string
	ParentRunID string
	RootRunID   string
	Depth       int
}

type storedRunEdge struct {
	ParentRunID string
	ChildRunID  string
	RootRunID   string
	Depth       int
}

func lookupRunNode(ctx context.Context, tx Tx, runID string) (storedRunNode, bool, error) {
	var node storedRunNode
	var parent sql.NullString
	err := tx.QueryRow(ctx, `SELECT id, parent_run_id, root_run_id, depth FROM runs WHERE id = ?`, runID).Scan(&node.RunID, &parent, &node.RootRunID, &node.Depth)
	if err != nil {
		if err == sql.ErrNoRows {
			return storedRunNode{}, false, nil
		}
		return storedRunNode{}, false, fmt.Errorf("inspect run %q: %w", runID, err)
	}
	node.ParentRunID = strings.TrimSpace(parent.String)
	return node, true, nil
}

func lookupRunEdge(ctx context.Context, tx Tx, parentRunID, childRunID string) (storedRunEdge, bool, error) {
	var edge storedRunEdge
	err := tx.QueryRow(ctx, `SELECT parent_run_id, child_run_id, root_run_id, depth FROM run_edges WHERE parent_run_id = ? AND child_run_id = ?`, parentRunID, childRunID).Scan(&edge.ParentRunID, &edge.ChildRunID, &edge.RootRunID, &edge.Depth)
	if err != nil {
		if err == sql.ErrNoRows {
			return storedRunEdge{}, false, nil
		}
		return storedRunEdge{}, false, fmt.Errorf("inspect run edge %q/%q: %w", parentRunID, childRunID, err)
	}
	return edge, true, nil
}

func currentRunStatus(ctx context.Context, tx Tx, runID string) (string, bool, error) {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = ?`, runID).Scan(&status)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", false, nil
		}
		return "", false, fmt.Errorf("inspect run %q status: %w", runID, err)
	}
	return normalizeDurableStatus(status), true, nil
}

func currentRunEdgeStatus(ctx context.Context, tx Tx, parentRunID, childRunID string) (string, bool, error) {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM run_edges WHERE parent_run_id = ? AND child_run_id = ?`, parentRunID, childRunID).Scan(&status)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", false, nil
		}
		return "", false, fmt.Errorf("inspect run edge %q/%q status: %w", parentRunID, childRunID, err)
	}
	return normalizeDurableStatus(status), true, nil
}

func currentRunClaim(ctx context.Context, tx Tx, runID string) (ClaimResult, bool, error) {
	var claim ClaimResult
	err := tx.QueryRow(ctx, `SELECT run_id, executor_id, claim_generation, claimed_at, lease_expires_at, heartbeat_at, phase, provider_idempotency_key, provider_receipt
		FROM run_claims WHERE run_id = ?`, runID).Scan(
		&claim.RunID,
		&claim.ExecutorID,
		&claim.ClaimGeneration,
		&claim.ClaimedAt,
		&claim.LeaseExpiresAt,
		&claim.HeartbeatAt,
		&claim.ClaimPhase,
		&claim.ProviderKey,
		&claim.ProviderReceipt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return ClaimResult{}, false, nil
		}
		return ClaimResult{}, false, fmt.Errorf("inspect run claim %q: %w", runID, err)
	}
	claim.ClaimPhase = normalizeClaimPhase(claim.ClaimPhase)
	return claim, true, nil
}

func normalizeClaimPhase(phase string) string {
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case ClaimPhaseClaimed, "":
		return ClaimPhaseClaimed
	case ClaimPhaseLaunching:
		return ClaimPhaseLaunching
	case ClaimPhaseExecuting:
		return ClaimPhaseExecuting
	case ClaimPhaseCompleted, "complete", "finished":
		return ClaimPhaseCompleted
	default:
		return strings.ToLower(strings.TrimSpace(phase))
	}
}

func claimPhaseAutoTakeoverAllowed(phase string) bool {
	return normalizeClaimPhase(phase) == ClaimPhaseClaimed
}

func providerIdempotencyKey(runID string) string {
	return "child-run:" + strings.TrimSpace(runID)
}

type nestedSchedulerResource struct {
	kind  string
	key   string
	limit int
}

func reserveNestedSchedulerResourcesTx(ctx context.Context, tx Tx, claim ClaimResult, parentRunID string, req SchedulerResourceReservationRequest, now, leaseUntil string) error {
	resources := nestedSchedulerResources(claim.RunID, parentRunID, req)
	for _, resource := range resources {
		if resource.limit <= 0 {
			continue
		}
		var active int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM nested_scheduler_resource_reservations
			WHERE resource_kind = ? AND resource_key = ? AND state = ? AND lease_expires_at > ?
				AND NOT (run_id = ? AND claim_generation = ?)`,
			resource.kind, resource.key, schedulerReservationStateActive, now, claim.RunID, claim.ClaimGeneration).Scan(&active); err != nil {
			return fmt.Errorf("reserve nested scheduler resource: count %s/%s: %w", resource.kind, resource.key, err)
		}
		if active >= resource.limit {
			return fmt.Errorf("%w: %s %s active=%d limit=%d", ErrNestedSchedulerResourceExhausted, resource.kind, resource.key, active, resource.limit)
		}
		reservationID := stableID("nsres_", claim.RunID, resource.kind, resource.key)
		if _, err := tx.Exec(ctx, `INSERT INTO nested_scheduler_resource_reservations(
				reservation_id, run_id, parent_run_id, root_run_id, resource_kind, resource_key,
				executor_id, claim_generation, join_identity, state, lease_expires_at, heartbeat_at,
				created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(reservation_id) DO UPDATE SET
				parent_run_id = excluded.parent_run_id,
				root_run_id = excluded.root_run_id,
				executor_id = excluded.executor_id,
				claim_generation = excluded.claim_generation,
				join_identity = excluded.join_identity,
				state = excluded.state,
				lease_expires_at = excluded.lease_expires_at,
				heartbeat_at = excluded.heartbeat_at,
				updated_at = excluded.updated_at`,
			reservationID, claim.RunID, parentRunID, firstNonEmptyNestedGraph(req.RootRunID, parentRunID),
			resource.kind, resource.key, claim.ExecutorID, claim.ClaimGeneration, claim.ProviderKey,
			schedulerReservationStateActive, leaseUntil, now, now, now); err != nil {
			return fmt.Errorf("reserve nested scheduler resource %s/%s: %w", resource.kind, resource.key, err)
		}
	}
	return nil
}

func nestedSchedulerResources(childRunID, parentRunID string, req SchedulerResourceReservationRequest) []nestedSchedulerResource {
	rootRunID := firstNonEmptyNestedGraph(req.RootRunID, parentRunID)
	providerKey := strings.TrimSpace(req.ProviderKey)
	if providerKey == "" {
		providerKey = "provider:default"
	}
	parentLimit := req.ParentMaxConcurrency
	rootLimit := req.RootMaxConcurrency
	if rootLimit <= 0 {
		rootLimit = parentLimit
	}
	providerLimit := req.ProviderMaxConcurrency
	if providerLimit <= 0 {
		providerLimit = parentLimit
	}
	resources := []nestedSchedulerResource{
		{kind: "root", key: rootRunID, limit: rootLimit},
		{kind: "parent", key: parentRunID, limit: parentLimit},
		{kind: "provider", key: providerKey, limit: providerLimit},
	}
	if parentRunID == "" || childRunID == "" {
		return nil
	}
	out := resources[:0]
	for _, resource := range resources {
		if strings.TrimSpace(resource.key) != "" && resource.limit > 0 {
			out = append(out, resource)
		}
	}
	return out
}

type nestedBudgetPolicy struct {
	id            string
	scopeKey      string
	mode          string
	ceiling       int64
	reserved      int64
	committed     int64
	recordVersion int64
	quantityKind  string
	unit          string
	valueScale    int
	windowKind    string
}

type nestedBudgetScope struct {
	ScopeKind         string `json:"scope_kind"`
	ProjectID         string `json:"project_id,omitempty"`
	DeliveryRunID     string `json:"delivery_run_id,omitempty"`
	TaskID            string `json:"task_id,omitempty"`
	WorkerID          string `json:"worker_id,omitempty"`
	SubAgentID        string `json:"sub_agent_id,omitempty"`
	AdapterID         string `json:"adapter_id,omitempty"`
	AccountProfileID  string `json:"account_profile_id,omitempty"`
	ModelCapabilityID string `json:"model_capability_id,omitempty"`
}

func reserveNestedSchedulerBudgetTx(ctx context.Context, tx Tx, claim ClaimResult, req SchedulerResourceReservationRequest, now, leaseUntil string) ([]AgentBudgetBinding, error) {
	req = normalizeNestedSchedulerBudgetRequest(req, claim.RunID)
	if req.BudgetRequestedValue <= 0 {
		if req.RequireBudgetAuthority || nestedSchedulerBudgetAuthorityRequired(req) {
			return nil, federationError(ErrChildBudgetRequiredCode, "positive child budget reservation is required for route %s", req.RoutingDecisionID)
		}
		return nil, nil
	}
	if err := validateNestedSchedulerBudgetAuthorityTx(ctx, tx, req); err != nil {
		return nil, err
	}
	scopes, terminalScope, err := nestedSchedulerBudgetScopes(req)
	if err != nil {
		return nil, err
	}
	policies, err := loadNestedSchedulerBudgetPoliciesTx(ctx, tx, scopes, req.BudgetQuantityKind, req.BudgetWindowKind)
	if err != nil {
		return nil, err
	}
	if len(policies) == 0 {
		return nil, federationError(ErrChildBudgetRequiredCode, "no applicable budget policy for route %s", req.RoutingDecisionID)
	}
	for _, policy := range policies {
		available := policy.ceiling - policy.reserved - policy.committed
		if req.BudgetRequestedValue > available && policy.mode == "hard" {
			return nil, federationError(ErrChildBudgetRequiredCode, "budget policy %s available=%d requested=%d", policy.id, available, req.BudgetRequestedValue)
		}
	}
	policyIDs := make([]string, 0, len(policies))
	for _, policy := range policies {
		policyIDs = append(policyIDs, policy.id)
	}
	sort.Strings(policyIDs)
	idempotencyKey := strings.Join([]string{"nested-scheduler-budget", claim.RunID, fmt.Sprint(claim.ClaimGeneration), req.RoutingDecisionID, req.BudgetQuantityKind, req.BudgetWindowKind}, ":")
	fingerprint := digestJSON(map[string]any{
		"schema_version":            "loopcoder.nested_scheduler_budget_request.v1",
		"run_id":                    claim.RunID,
		"claim_generation":          claim.ClaimGeneration,
		"project_id":                req.ProjectID,
		"delivery_run_id":           req.DeliveryRunID,
		"task_id":                   req.TaskID,
		"sub_agent_id":              req.SubAgentID,
		"adapter_id":                req.AdapterID,
		"provider_installation_id":  req.ProviderInstallationID,
		"account_profile_id":        req.AccountProfileID,
		"model_capability_id":       req.ModelCapabilityID,
		"routing_decision_id":       req.RoutingDecisionID,
		"routing_fingerprint":       req.RoutingFingerprint,
		"plan_fingerprint":          req.PlanFingerprint,
		"policy_fingerprint":        req.PolicyFingerprint,
		"authorization_fingerprint": req.AuthorizationFingerprint,
		"quantity_kind":             req.BudgetQuantityKind,
		"unit":                      req.BudgetUnit,
		"value_scale":               req.BudgetValueScale,
		"window_kind":               req.BudgetWindowKind,
		"requested_value":           req.BudgetRequestedValue,
		"lease_expires_at":          leaseUntil,
		"policy_ids":                policyIDs,
	})
	reservationID := stableID("bres_", idempotencyKey, policyIDs[0], req.SubAgentID, fingerprint)
	existing, replay, err := nestedSchedulerBudgetReplayTx(ctx, tx, claim, req, idempotencyKey, policyIDs[0], fingerprint, policyIDs, now)
	if err != nil {
		return nil, err
	}
	if replay {
		return nestedSchedulerBudgetBindings(req, existing, policyIDs, now), nil
	}
	scopeKey, err := nestedSchedulerBudgetScopeKey(terminalScope)
	if err != nil {
		return nil, err
	}
	policyIDsJSON, err := json.Marshal(policyIDs)
	if err != nil {
		return nil, err
	}
	emptyJSON, _ := json.Marshal([]string{})
	payload := map[string]any{
		"schema_version":                     "loopcoder.budget_reservation.v1",
		"record_version":                     1,
		"budget_reservation_id":              reservationID,
		"idempotency_key":                    idempotencyKey,
		"request_fingerprint":                fingerprint,
		"requester_id":                       req.SubAgentID,
		"authorization_fingerprint":          req.AuthorizationFingerprint,
		"scope":                              terminalScope,
		"scope_key":                          scopeKey,
		"quantity_kind":                      req.BudgetQuantityKind,
		"unit":                               req.BudgetUnit,
		"value_scale":                        req.BudgetValueScale,
		"requested_value":                    req.BudgetRequestedValue,
		"reserved_value":                     req.BudgetRequestedValue,
		"committed_value":                    0,
		"released_value":                     0,
		"state":                              BudgetReservationStateActive,
		"generation":                         1,
		"lease_expires_at":                   leaseUntil,
		"budget_policy_ids":                  policyIDs,
		"requirement_confidence":             "exact",
		"created_at":                         now,
		"updated_at":                         now,
		"created_by":                         map[string]string{"actor_id": "nested-scheduler", "role": "scheduler"},
		"updated_by":                         map[string]string{"actor_id": "nested-scheduler", "role": "scheduler"},
		"host":                               map[string]string{"provider": "loopcoder", "model": "nested-scheduler"},
		"gap_reasons":                        []string{},
		"scheduler_executor_id":              claim.ExecutorID,
		"scheduler_claim_generation":         claim.ClaimGeneration,
		"scheduler_provider_key":             req.ProviderKey,
		"scheduler_provider_installation_id": req.ProviderInstallationID,
		"scheduler_routing_decision_id":      req.RoutingDecisionID,
		"scheduler_routing_fingerprint":      req.RoutingFingerprint,
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO budget_reservations(
			budget_reservation_id, idempotency_key, request_fingerprint, requester_id, authorization_fingerprint,
			project_id, delivery_run_id, task_id, worker_id, sub_agent_id, adapter_id, account_profile_id, model_capability_id,
			quantity_kind, unit, value_scale, requested_value, reserved_value, committed_value, released_value, state,
			generation, lease_expires_at, scope_key, policy_ids_json, source_estimate_usage_record_ids_json,
			commit_usage_record_ids_json, release_usage_record_ids_json, payload_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		reservationID, idempotencyKey, fingerprint, req.SubAgentID, req.AuthorizationFingerprint,
		req.ProjectID, req.DeliveryRunID, req.TaskID, req.SubAgentID, req.AdapterID, req.AccountProfileID, req.ModelCapabilityID,
		req.BudgetQuantityKind, req.BudgetUnit, req.BudgetValueScale, req.BudgetRequestedValue, req.BudgetRequestedValue,
		BudgetReservationStateActive, leaseUntil, scopeKey, string(policyIDsJSON), string(emptyJSON), string(emptyJSON), string(emptyJSON), string(payloadJSON), now, now); err != nil {
		if IsConstraint(err) {
			return nil, federationError(ErrDuplicateReplayCode, "budget reservation replay conflicted for %s", idempotencyKey)
		}
		return nil, fmt.Errorf("reserve nested scheduler budget: %w", err)
	}
	for _, policyID := range policyIDs {
		policy := nestedSchedulerPolicyByID(policies, policyID)
		result, err := tx.Exec(ctx, `UPDATE budget_aggregates
			SET reserved_value = reserved_value + ?, record_version = record_version + 1, updated_at = ?
			WHERE budget_policy_id = ?
				AND record_version = ?
				AND reserved_value + ? >= 0
				AND committed_value >= 0
				AND (? <> 'hard' OR reserved_value + committed_value + ? <= ?)`,
			req.BudgetRequestedValue, now, policyID, policy.recordVersion, req.BudgetRequestedValue, policy.mode, req.BudgetRequestedValue, policy.ceiling)
		if err != nil {
			return nil, fmt.Errorf("reserve nested scheduler budget aggregate %s: %w", policyID, err)
		}
		affected, err := result.RowsAffected()
		if err == nil && affected != 1 {
			return nil, federationError(ErrChildBudgetRequiredCode, "reserved %d aggregate rows for %s, want 1", affected, policyID)
		}
	}
	return nestedSchedulerBudgetBindings(req, reservationID, policyIDs, now), nil
}

func nestedSchedulerBudgetAuthorityRequired(req SchedulerResourceReservationRequest) bool {
	return strings.TrimSpace(req.ProjectID) != "" ||
		strings.TrimSpace(req.DeliveryRunID) != "" ||
		strings.TrimSpace(req.TaskID) != "" ||
		strings.TrimSpace(req.AdapterID) != "" ||
		strings.TrimSpace(req.ProviderInstallationID) != "" ||
		strings.TrimSpace(req.AccountProfileID) != "" ||
		strings.TrimSpace(req.ModelCapabilityID) != "" ||
		strings.TrimSpace(req.RoutingDecisionID) != "" ||
		strings.TrimSpace(req.RoutingFingerprint) != "" ||
		strings.TrimSpace(req.PlanFingerprint) != "" ||
		strings.TrimSpace(req.PolicyFingerprint) != "" ||
		strings.TrimSpace(req.AuthorizationFingerprint) != "" ||
		req.AttachBudgetBinding
}

func normalizeNestedSchedulerBudgetRequest(req SchedulerResourceReservationRequest, childRunID string) SchedulerResourceReservationRequest {
	req.ProjectID = strings.TrimSpace(req.ProjectID)
	req.DeliveryRunID = strings.TrimSpace(req.DeliveryRunID)
	req.TaskID = strings.TrimSpace(req.TaskID)
	req.SubAgentID = firstNonEmptyNestedGraph(strings.TrimSpace(req.SubAgentID), strings.TrimSpace(childRunID))
	req.AdapterID = strings.ToLower(strings.TrimSpace(req.AdapterID))
	req.ProviderInstallationID = strings.TrimSpace(req.ProviderInstallationID)
	req.AccountProfileID = strings.TrimSpace(req.AccountProfileID)
	req.ModelCapabilityID = strings.TrimSpace(req.ModelCapabilityID)
	req.RoutingDecisionID = strings.TrimSpace(req.RoutingDecisionID)
	req.RoutingFingerprint = strings.TrimSpace(req.RoutingFingerprint)
	req.PlanFingerprint = strings.TrimSpace(req.PlanFingerprint)
	req.PolicyFingerprint = strings.TrimSpace(req.PolicyFingerprint)
	req.AuthorizationFingerprint = strings.TrimSpace(req.AuthorizationFingerprint)
	req.BudgetQuantityKind = firstNonEmptyNestedGraph(strings.TrimSpace(req.BudgetQuantityKind), "local-policy")
	req.BudgetUnit = firstNonEmptyNestedGraph(strings.TrimSpace(req.BudgetUnit), nestedBudgetUnit(req.BudgetQuantityKind))
	req.BudgetWindowKind = firstNonEmptyNestedGraph(strings.TrimSpace(req.BudgetWindowKind), "unbounded")
	return req
}

func validateNestedSchedulerBudgetAuthorityTx(ctx context.Context, tx Tx, req SchedulerResourceReservationRequest) error {
	if req.ProjectID == "" || req.DeliveryRunID == "" || req.TaskID == "" || req.SubAgentID == "" || req.AdapterID == "" ||
		req.RoutingDecisionID == "" || req.RoutingFingerprint == "" || req.PlanFingerprint == "" || req.PolicyFingerprint == "" || req.AuthorizationFingerprint == "" {
		return federationError(ErrChildBudgetRequiredCode, "accepted route, provider, policy, authorization, and child budget authority are required")
	}
	var projectID, deliveryRunID, taskID, planFingerprint, policyFingerprint, routingFingerprint, status, chosen, eligibleJSON, scoredJSON string
	err := tx.QueryRow(ctx, `SELECT project_id, delivery_run_id, task_id, plan_fingerprint, policy_fingerprint, routing_fingerprint, decision_status, chosen_candidate_id, eligible_candidates_json, scored_candidates_json
		FROM routing_decisions WHERE routing_decision_id = ?`, req.RoutingDecisionID).Scan(
		&projectID, &deliveryRunID, &taskID, &planFingerprint, &policyFingerprint, &routingFingerprint, &status, &chosen, &eligibleJSON, &scoredJSON)
	if err != nil {
		if err == sql.ErrNoRows {
			return federationError(ErrChildBudgetRequiredCode, "accepted routing decision %s is missing", req.RoutingDecisionID)
		}
		return fmt.Errorf("validate nested scheduler route: %w", err)
	}
	if projectID != req.ProjectID || deliveryRunID != req.DeliveryRunID || taskID != req.TaskID ||
		planFingerprint != req.PlanFingerprint || policyFingerprint != req.PolicyFingerprint || routingFingerprint != req.RoutingFingerprint ||
		status != "selected" || strings.TrimSpace(chosen) == "" {
		return federationError(ErrChildBudgetRequiredCode, "routing decision %s is not accepted for this child budget", req.RoutingDecisionID)
	}
	candidate, ok := nestedSchedulerChosenCandidate(chosen, eligibleJSON, scoredJSON)
	if !ok {
		return federationError(ErrChildBudgetRequiredCode, "routing decision %s chosen candidate %s is not persisted", req.RoutingDecisionID, chosen)
	}
	if !nestedSchedulerCandidateMatchesRequest(candidate, req) {
		return federationError(ErrChildBudgetRequiredCode, "routing decision %s chosen candidate %s does not match requested provider route", req.RoutingDecisionID, chosen)
	}
	return nil
}

type nestedSchedulerRouteCandidate struct {
	RoutingCandidateID     string `json:"routing_candidate_id"`
	TaskID                 string `json:"task_id"`
	AdapterID              string `json:"adapter_id"`
	ProviderInstallationID string `json:"provider_installation_id"`
	AccountProfileID       string `json:"account_profile_id"`
	ModelCapabilityID      string `json:"model_capability_id"`
	InvocationProfileKey   string `json:"invocation_profile_key"`
	CandidateFingerprint   string `json:"candidate_fingerprint"`
}

type nestedSchedulerScoredCandidate struct {
	RoutingCandidateID string                        `json:"routing_candidate_id"`
	Candidate          nestedSchedulerRouteCandidate `json:"candidate"`
}

func nestedSchedulerChosenCandidate(chosen, eligibleJSON, scoredJSON string) (nestedSchedulerRouteCandidate, bool) {
	chosen = strings.TrimSpace(chosen)
	var eligible []nestedSchedulerRouteCandidate
	if err := json.Unmarshal([]byte(firstNonEmptyNestedGraph(eligibleJSON, "[]")), &eligible); err == nil {
		for _, candidate := range eligible {
			if strings.TrimSpace(candidate.RoutingCandidateID) == chosen {
				return candidate, true
			}
		}
	}
	var scored []nestedSchedulerScoredCandidate
	if err := json.Unmarshal([]byte(firstNonEmptyNestedGraph(scoredJSON, "[]")), &scored); err == nil {
		for _, scoredCandidate := range scored {
			candidate := scoredCandidate.Candidate
			if strings.TrimSpace(candidate.RoutingCandidateID) == "" {
				candidate.RoutingCandidateID = scoredCandidate.RoutingCandidateID
			}
			if strings.TrimSpace(candidate.RoutingCandidateID) == chosen {
				return candidate, true
			}
		}
	}
	return nestedSchedulerRouteCandidate{}, false
}

func nestedSchedulerCandidateMatchesRequest(candidate nestedSchedulerRouteCandidate, req SchedulerResourceReservationRequest) bool {
	return strings.TrimSpace(candidate.TaskID) == req.TaskID &&
		strings.EqualFold(strings.TrimSpace(candidate.AdapterID), req.AdapterID) &&
		strings.TrimSpace(candidate.ProviderInstallationID) == req.ProviderInstallationID &&
		strings.TrimSpace(candidate.AccountProfileID) == req.AccountProfileID &&
		strings.TrimSpace(candidate.ModelCapabilityID) == req.ModelCapabilityID
}

func nestedSchedulerBudgetScopes(req SchedulerResourceReservationRequest) ([]nestedBudgetScope, nestedBudgetScope, error) {
	scopes := []nestedBudgetScope{
		{ScopeKind: "project", ProjectID: req.ProjectID},
		{ScopeKind: "delivery-run", ProjectID: req.ProjectID, DeliveryRunID: req.DeliveryRunID},
		{ScopeKind: "task", ProjectID: req.ProjectID, DeliveryRunID: req.DeliveryRunID, TaskID: req.TaskID},
		{ScopeKind: "sub-agent", ProjectID: req.ProjectID, DeliveryRunID: req.DeliveryRunID, TaskID: req.TaskID, SubAgentID: req.SubAgentID, AdapterID: req.AdapterID, AccountProfileID: req.AccountProfileID, ModelCapabilityID: req.ModelCapabilityID},
	}
	if req.AdapterID != "" {
		scopes = append(scopes, nestedBudgetScope{ScopeKind: "provider-scope", ProjectID: req.ProjectID, AdapterID: req.AdapterID, AccountProfileID: req.AccountProfileID, ModelCapabilityID: req.ModelCapabilityID})
	}
	terminal := scopes[3]
	for _, scope := range scopes {
		if _, err := nestedSchedulerBudgetScopeKey(scope); err != nil {
			return nil, nestedBudgetScope{}, err
		}
	}
	return scopes, terminal, nil
}

func nestedSchedulerBudgetScopeKey(scope nestedBudgetScope) (string, error) {
	if scope.ScopeKind != "machine" && strings.TrimSpace(scope.ProjectID) == "" {
		return "", federationError(ErrChildBudgetRequiredCode, "project_id is required for %s budget scope", scope.ScopeKind)
	}
	data, err := json.Marshal(scope)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func loadNestedSchedulerBudgetPoliciesTx(ctx context.Context, tx Tx, scopes []nestedBudgetScope, quantityKind, windowKind string) ([]nestedBudgetPolicy, error) {
	var policies []nestedBudgetPolicy
	seen := map[string]bool{}
	for _, scope := range scopes {
		key, err := nestedSchedulerBudgetScopeKey(scope)
		if err != nil {
			return nil, err
		}
		rows, err := tx.Query(ctx, `SELECT p.budget_policy_id, p.scope_key, p.policy_mode, p.ceiling_value,
				a.reserved_value, a.committed_value, p.quantity_kind, p.unit, p.value_scale, p.window_kind, a.record_version
			FROM budget_policies p
			LEFT JOIN budget_aggregates a ON a.budget_policy_id = p.budget_policy_id
			WHERE p.scope_key = ? AND p.quantity_kind = ? AND p.window_kind = ? AND p.active = 1
			ORDER BY p.budget_policy_id`, key, quantityKind, windowKind)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var policy nestedBudgetPolicy
			var reserved, committed, recordVersion sql.NullInt64
			if err := rows.Scan(&policy.id, &policy.scopeKey, &policy.mode, &policy.ceiling, &reserved, &committed, &policy.quantityKind, &policy.unit, &policy.valueScale, &policy.windowKind, &recordVersion); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if !reserved.Valid || !committed.Valid || !recordVersion.Valid {
				_ = rows.Close()
				return nil, federationError(ErrChildBudgetRequiredCode, "budget aggregate for policy %s is missing", policy.id)
			}
			if reserved.Int64 < 0 || committed.Int64 < 0 || recordVersion.Int64 <= 0 {
				_ = rows.Close()
				return nil, federationError(ErrChildBudgetRequiredCode, "budget aggregate for policy %s is invalid", policy.id)
			}
			policy.reserved = reserved.Int64
			policy.committed = committed.Int64
			policy.recordVersion = recordVersion.Int64
			if seen[policy.id] {
				continue
			}
			seen[policy.id] = true
			policies = append(policies, policy)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	sort.Slice(policies, func(i, j int) bool {
		if policies[i].scopeKey == policies[j].scopeKey {
			return policies[i].id < policies[j].id
		}
		return policies[i].scopeKey < policies[j].scopeKey
	})
	return policies, nil
}

func nestedSchedulerPolicyByID(policies []nestedBudgetPolicy, policyID string) nestedBudgetPolicy {
	for _, policy := range policies {
		if policy.id == policyID {
			return policy
		}
	}
	return nestedBudgetPolicy{id: policyID}
}

func nestedSchedulerBudgetReplayTx(ctx context.Context, tx Tx, claim ClaimResult, req SchedulerResourceReservationRequest, idempotencyKey, primaryPolicyID, fingerprint string, policyIDs []string, now string) (string, bool, error) {
	var reservation nestedSchedulerHeldBudgetReservation
	var existingFingerprint, state, policyIDsJSON, payloadJSON string
	err := tx.QueryRow(ctx, `SELECT budget_reservation_id, request_fingerprint, policy_ids_json, state,
			project_id, delivery_run_id, task_id, sub_agent_id, adapter_id, account_profile_id, model_capability_id, authorization_fingerprint,
			quantity_kind, unit, value_scale, requested_value, generation, lease_expires_at, payload_json
		FROM budget_reservations
		WHERE idempotency_key = ? AND COALESCE(json_extract(policy_ids_json, '$[0]'), '') = ?`,
		idempotencyKey, primaryPolicyID).Scan(
		&reservation.id, &existingFingerprint, &policyIDsJSON, &state,
		&reservation.projectID, &reservation.deliveryRunID, &reservation.taskID, &reservation.subAgentID, &reservation.adapterID, &reservation.accountProfileID, &reservation.modelCapabilityID, &reservation.authorizationFingerprint,
		&reservation.quantityKind, &reservation.unit, &reservation.valueScale, &reservation.requested, &reservation.generation, &reservation.leaseExpiresAt, &payloadJSON)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", false, nil
		}
		return "", false, err
	}
	if existingFingerprint != fingerprint {
		return "", false, federationError(ErrDuplicateReplayCode, "budget reservation idempotency key replayed with different request")
	}
	if state != BudgetReservationStateActive && state != BudgetReservationStatePartiallyCommitted {
		return "", false, federationError(ErrChildBudgetRequiredCode, "budget reservation %s is %s", reservation.id, state)
	}
	reservation.policyIDs = decodeStringList(policyIDsJSON)
	if !sameStringSet(reservation.policyIDs, policyIDs) {
		return "", false, federationError(ErrChildBudgetRequiredCode, "budget reservation %s policy set does not match route", reservation.id)
	}
	if !claimLeaseActive(reservation.leaseExpiresAt, now) {
		return "", false, federationError(ErrChildBudgetRequiredCode, "budget reservation %s lease is not live", reservation.id)
	}
	if err := reservation.loadSchedulerPayload(payloadJSON); err != nil {
		return "", false, err
	}
	if err := validateNestedSchedulerHeldBudgetReservation(ctx, tx, reservation, claim.RunID, claim.ExecutorID, claim.ClaimGeneration, req, now); err != nil {
		return "", false, err
	}
	return reservation.id, true, nil
}

func nestedSchedulerBudgetBindings(req SchedulerResourceReservationRequest, reservationID string, policyIDs []string, at string) []AgentBudgetBinding {
	if !req.AttachBudgetBinding {
		return nil
	}
	bindings := make([]AgentBudgetBinding, 0, len(policyIDs))
	for _, policyID := range policyIDs {
		binding := AgentBudgetBinding{
			SchemaVersion:          AgentBudgetBindingSchema,
			ProjectID:              req.ProjectID,
			DeliveryRunID:          req.DeliveryRunID,
			ChildAgentID:           req.SubAgentID,
			BudgetPolicyID:         policyID,
			BudgetReservationID:    reservationID,
			ReservationScope:       "sub-agent",
			ReservedQuantitiesJSON: "{}",
			AncestorBudgetRefsJSON: "[]",
			ReservationState:       BudgetReservationStateActive,
			CreatedAt:              at,
			UpdatedAt:              at,
		}
		binding.AgentBudgetBindingID = stableID("abudget_", binding.ChildAgentID, binding.BudgetReservationID, binding.BudgetPolicyID)
		bindings = append(bindings, binding)
	}
	return bindings
}

type nestedSchedulerHeldBudgetReservation struct {
	id                       string
	projectID                string
	deliveryRunID            string
	taskID                   string
	subAgentID               string
	adapterID                string
	providerInstallationID   string
	accountProfileID         string
	modelCapabilityID        string
	authorizationFingerprint string
	quantityKind             string
	unit                     string
	valueScale               int
	requested                int64
	reserved                 int64
	committed                int64
	released                 int64
	generation               int64
	leaseExpiresAt           string
	policyIDs                []string
	schedulerExecutorID      string
	schedulerClaimGeneration int64
	schedulerProviderKey     string
	schedulerRoutingID       string
	schedulerRoutingFP       string
}

func (r *nestedSchedulerHeldBudgetReservation) loadSchedulerPayload(payloadJSON string) error {
	var payload struct {
		SchedulerExecutorID         string `json:"scheduler_executor_id"`
		SchedulerClaimGeneration    int64  `json:"scheduler_claim_generation"`
		SchedulerProviderKey        string `json:"scheduler_provider_key"`
		SchedulerProviderInstallID  string `json:"scheduler_provider_installation_id"`
		SchedulerRoutingDecisionID  string `json:"scheduler_routing_decision_id"`
		SchedulerRoutingFingerprint string `json:"scheduler_routing_fingerprint"`
	}
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		return fmt.Errorf("nested scheduler budget payload: %w", err)
	}
	r.schedulerExecutorID = strings.TrimSpace(payload.SchedulerExecutorID)
	r.schedulerClaimGeneration = payload.SchedulerClaimGeneration
	r.schedulerProviderKey = strings.TrimSpace(payload.SchedulerProviderKey)
	r.providerInstallationID = strings.TrimSpace(payload.SchedulerProviderInstallID)
	r.schedulerRoutingID = strings.TrimSpace(payload.SchedulerRoutingDecisionID)
	r.schedulerRoutingFP = strings.TrimSpace(payload.SchedulerRoutingFingerprint)
	return nil
}

func (r nestedSchedulerHeldBudgetReservation) request() SchedulerResourceReservationRequest {
	return SchedulerResourceReservationRequest{
		ProviderKey:              r.schedulerProviderKey,
		ProjectID:                r.projectID,
		DeliveryRunID:            r.deliveryRunID,
		TaskID:                   r.taskID,
		SubAgentID:               r.subAgentID,
		AdapterID:                r.adapterID,
		ProviderInstallationID:   r.providerInstallationID,
		AccountProfileID:         r.accountProfileID,
		ModelCapabilityID:        r.modelCapabilityID,
		RoutingDecisionID:        r.schedulerRoutingID,
		RoutingFingerprint:       r.schedulerRoutingFP,
		AuthorizationFingerprint: r.authorizationFingerprint,
		BudgetRequestedValue:     r.requested,
		BudgetQuantityKind:       r.quantityKind,
		BudgetUnit:               r.unit,
		BudgetValueScale:         r.valueScale,
	}
}

func validateNestedSchedulerHeldBudgetReservation(ctx context.Context, tx Tx, item nestedSchedulerHeldBudgetReservation, childRunID, executorID string, claimGeneration int64, req SchedulerResourceReservationRequest, now string) error {
	if item.subAgentID != childRunID || item.schedulerExecutorID != executorID || item.schedulerClaimGeneration != claimGeneration {
		return federationError(ErrChildBudgetRequiredCode, "budget reservation %s is not fenced to this scheduler claim", item.id)
	}
	if item.requested <= 0 || item.reserved < 0 || item.committed < 0 || item.released < 0 || len(item.policyIDs) == 0 {
		return federationError(ErrChildBudgetRequiredCode, "budget reservation %s has invalid accounting authority", item.id)
	}
	if !claimLeaseActive(item.leaseExpiresAt, now) {
		return federationError(ErrChildBudgetRequiredCode, "budget reservation %s lease is not live", item.id)
	}
	if req.ProjectID != item.projectID || req.DeliveryRunID != item.deliveryRunID || req.TaskID != item.taskID ||
		req.SubAgentID != item.subAgentID || !strings.EqualFold(req.AdapterID, item.adapterID) ||
		req.ProviderInstallationID != item.providerInstallationID || req.AccountProfileID != item.accountProfileID ||
		req.ModelCapabilityID != item.modelCapabilityID || req.AuthorizationFingerprint != item.authorizationFingerprint ||
		req.BudgetRequestedValue != item.requested || req.BudgetQuantityKind != item.quantityKind ||
		req.BudgetUnit != item.unit || req.BudgetValueScale != item.valueScale {
		return federationError(ErrChildBudgetRequiredCode, "budget reservation %s does not match requested authority", item.id)
	}
	if req.RoutingDecisionID != item.schedulerRoutingID || req.RoutingFingerprint != item.schedulerRoutingFP {
		return federationError(ErrChildBudgetRequiredCode, "budget reservation %s route binding does not match scheduler claim", item.id)
	}
	req.PolicyFingerprint = strings.TrimSpace(req.PolicyFingerprint)
	req.PlanFingerprint = strings.TrimSpace(req.PlanFingerprint)
	if req.PolicyFingerprint == "" || req.PlanFingerprint == "" {
		var planFingerprint, policyFingerprint string
		if err := tx.QueryRow(ctx, `SELECT plan_fingerprint, policy_fingerprint FROM routing_decisions WHERE routing_decision_id = ?`, req.RoutingDecisionID).Scan(&planFingerprint, &policyFingerprint); err != nil {
			if err == sql.ErrNoRows {
				return federationError(ErrChildBudgetRequiredCode, "accepted routing decision %s is missing", req.RoutingDecisionID)
			}
			return err
		}
		req.PlanFingerprint = planFingerprint
		req.PolicyFingerprint = policyFingerprint
	}
	return validateNestedSchedulerBudgetAuthorityTx(ctx, tx, req)
}

func loadNestedSchedulerHeldBudgetReservationsTx(ctx context.Context, tx Tx, childRunID, executorID string, claimGeneration int64, now string, unboundOnly bool) ([]nestedSchedulerHeldBudgetReservation, error) {
	notBound := ""
	if unboundOnly {
		notBound = `AND NOT EXISTS (SELECT 1 FROM agent_budget_bindings b WHERE b.budget_reservation_id = r.budget_reservation_id)`
	}
	rows, err := tx.Query(ctx, `SELECT budget_reservation_id, project_id, delivery_run_id, task_id, sub_agent_id, adapter_id,
			account_profile_id, model_capability_id, authorization_fingerprint, quantity_kind, unit, value_scale,
			requested_value, reserved_value, committed_value, released_value, generation, lease_expires_at, policy_ids_json, payload_json
		FROM budget_reservations r
		WHERE sub_agent_id = ?
			AND state IN ('active', 'partially-committed')
			AND lease_expires_at > ?
			AND json_extract(payload_json, '$.scheduler_executor_id') = ?
			AND json_extract(payload_json, '$.scheduler_claim_generation') = ? `+notBound+`
		ORDER BY budget_reservation_id`, childRunID, now, executorID, claimGeneration)
	if err != nil {
		return nil, err
	}
	var held []nestedSchedulerHeldBudgetReservation
	for rows.Next() {
		var item nestedSchedulerHeldBudgetReservation
		var policyIDsJSON, payloadJSON string
		if err := rows.Scan(&item.id, &item.projectID, &item.deliveryRunID, &item.taskID, &item.subAgentID, &item.adapterID,
			&item.accountProfileID, &item.modelCapabilityID, &item.authorizationFingerprint, &item.quantityKind, &item.unit, &item.valueScale,
			&item.requested, &item.reserved, &item.committed, &item.released, &item.generation, &item.leaseExpiresAt, &policyIDsJSON, &payloadJSON); err != nil {
			_ = rows.Close()
			return nil, err
		}
		item.policyIDs = decodeStringList(policyIDsJSON)
		if err := item.loadSchedulerPayload(payloadJSON); err != nil {
			_ = rows.Close()
			return nil, err
		}
		held = append(held, item)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return held, nil
}

func nestedSchedulerBudgetHistoryCountTx(ctx context.Context, tx Tx, childRunID string, claimGeneration int64) (int64, error) {
	var count int64
	err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM budget_reservations
		WHERE sub_agent_id = ? AND json_extract(payload_json, '$.scheduler_claim_generation') = ?`, childRunID, claimGeneration).Scan(&count)
	return count, err
}

func nestedSchedulerUnboundBudgetHistoryCountTx(ctx context.Context, tx Tx, childRunID string, claimGeneration int64) (int64, error) {
	var count int64
	err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM budget_reservations r
		WHERE sub_agent_id = ?
			AND json_extract(payload_json, '$.scheduler_claim_generation') = ?
			AND NOT EXISTS (SELECT 1 FROM agent_budget_bindings b WHERE b.budget_reservation_id = r.budget_reservation_id)`,
		childRunID, claimGeneration).Scan(&count)
	return count, err
}

func nestedSchedulerResourceHistoryCountTx(ctx context.Context, tx Tx, childRunID string, claimGeneration int64) (int64, error) {
	var count int64
	err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM nested_scheduler_resource_reservations WHERE run_id = ? AND claim_generation = ?`, childRunID, claimGeneration).Scan(&count)
	return count, err
}

func nestedSchedulerBudgetAggregateVersionTx(ctx context.Context, tx Tx, policyID string) (int64, error) {
	var recordVersion int64
	err := tx.QueryRow(ctx, `SELECT record_version FROM budget_aggregates WHERE budget_policy_id = ?`, policyID).Scan(&recordVersion)
	if err == sql.ErrNoRows {
		return 0, federationError(ErrChildBudgetRequiredCode, "budget aggregate for policy %s is missing", policyID)
	}
	return recordVersion, err
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy := append([]string(nil), left...)
	rightCopy := append([]string(nil), right...)
	sort.Strings(leftCopy)
	sort.Strings(rightCopy)
	for i := range leftCopy {
		if leftCopy[i] != rightCopy[i] {
			return false
		}
	}
	return true
}

func renewNestedSchedulerBudgetTx(ctx context.Context, tx Tx, childRunID, executorID string, claimGeneration int64, now, leaseUntil string) error {
	expected, err := nestedSchedulerBudgetHistoryCountTx(ctx, tx, childRunID, claimGeneration)
	if err != nil {
		return fmt.Errorf("renew nested scheduler budget: %w", err)
	}
	if expected == 0 {
		return nil
	}
	held, err := loadNestedSchedulerHeldBudgetReservationsTx(ctx, tx, childRunID, executorID, claimGeneration, now, false)
	if err != nil {
		return fmt.Errorf("renew nested scheduler budget: %w", err)
	}
	if int64(len(held)) != expected {
		return federationError(ErrChildBudgetRequiredCode, "renewed %d budget reservation rows for %s generation %d, want %d", len(held), childRunID, claimGeneration, expected)
	}
	for _, item := range held {
		if err := validateNestedSchedulerHeldBudgetReservation(ctx, tx, item, childRunID, executorID, claimGeneration, item.request(), now); err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `UPDATE budget_reservations
			SET lease_expires_at = ?,
				generation = generation + 1,
				updated_at = ?,
				payload_json = json_set(payload_json,
					'$.lease_expires_at', ?,
					'$.generation', generation + 1,
					'$.updated_at', ?)
			WHERE budget_reservation_id = ?
				AND generation = ?
				AND state IN ('active', 'partially-committed')
				AND lease_expires_at > ?
				AND json_extract(payload_json, '$.scheduler_executor_id') = ?
				AND json_extract(payload_json, '$.scheduler_claim_generation') = ?`,
			leaseUntil, now, leaseUntil, now, item.id, item.generation, now, executorID, claimGeneration)
		if err != nil {
			return fmt.Errorf("renew nested scheduler budget reservation %s: %w", item.id, err)
		}
		affected, err := result.RowsAffected()
		if err == nil && affected != 1 {
			return federationError(ErrChildBudgetRequiredCode, "renewed %d budget reservation rows for %s, want 1", affected, item.id)
		}
	}
	return nil
}

func releaseNestedSchedulerBudgetTx(ctx context.Context, tx Tx, childRunID, executorID string, claimGeneration int64, status, at string) error {
	expected, err := nestedSchedulerUnboundBudgetHistoryCountTx(ctx, tx, childRunID, claimGeneration)
	if err != nil {
		return fmt.Errorf("release nested scheduler budget: %w", err)
	}
	if expected == 0 {
		return nil
	}
	held, err := loadNestedSchedulerHeldBudgetReservationsTx(ctx, tx, childRunID, executorID, claimGeneration, at, true)
	if err != nil {
		return fmt.Errorf("release nested scheduler budget: %w", err)
	}
	if int64(len(held)) != expected {
		return federationError(ErrChildBudgetRequiredCode, "released %d budget reservation rows for %s generation %d, want %d", len(held), childRunID, claimGeneration, expected)
	}
	commit := status == "succeeded" || status == "succeeded_with_optional_failures"
	finalState := "cancelled"
	if commit {
		finalState = "committed"
	}
	for _, item := range held {
		if err := validateNestedSchedulerHeldBudgetReservation(ctx, tx, item, childRunID, executorID, claimGeneration, item.request(), at); err != nil {
			return err
		}
		nextCommitted := item.committed
		nextReleased := item.released
		deltaCommitted := int64(0)
		if commit {
			nextCommitted += item.reserved
			deltaCommitted = item.reserved
		} else {
			nextReleased += item.reserved
		}
		result, err := tx.Exec(ctx, `UPDATE budget_reservations
			SET reserved_value = 0,
				committed_value = ?,
				released_value = ?,
				state = ?,
				generation = generation + 1,
				updated_at = ?,
				payload_json = json_set(payload_json,
					'$.reserved_value', 0,
					'$.committed_value', ?,
					'$.released_value', ?,
					'$.state', ?,
					'$.generation', generation + 1,
					'$.updated_at', ?)
			WHERE budget_reservation_id = ?
				AND generation = ?
				AND reserved_value = ?
				AND state IN ('active', 'partially-committed')
				AND lease_expires_at > ?
				AND json_extract(payload_json, '$.scheduler_executor_id') = ?
				AND json_extract(payload_json, '$.scheduler_claim_generation') = ?`,
			nextCommitted, nextReleased, finalState, at,
			nextCommitted, nextReleased, finalState, at,
			item.id, item.generation, item.reserved, at, executorID, claimGeneration)
		if err != nil {
			return fmt.Errorf("release nested scheduler budget reservation %s: %w", item.id, err)
		}
		affected, err := result.RowsAffected()
		if err == nil && affected != 1 {
			return federationError(ErrChildBudgetRequiredCode, "budget reservation %s release affected %d rows", item.id, affected)
		}
		for _, policyID := range item.policyIDs {
			recordVersion, err := nestedSchedulerBudgetAggregateVersionTx(ctx, tx, policyID)
			if err != nil {
				return err
			}
			result, err := tx.Exec(ctx, `UPDATE budget_aggregates
				SET reserved_value = reserved_value - ?, committed_value = committed_value + ?, record_version = record_version + 1, updated_at = ?
				WHERE budget_policy_id = ?
					AND record_version = ?
					AND reserved_value - ? >= 0
					AND committed_value + ? >= 0`,
				item.reserved, deltaCommitted, at, policyID, recordVersion, item.reserved, deltaCommitted)
			if err != nil {
				return fmt.Errorf("release nested scheduler budget aggregate %s: %w", policyID, err)
			}
			affected, err := result.RowsAffected()
			if err == nil && affected != 1 {
				return federationError(ErrChildBudgetRequiredCode, "released %d aggregate rows for %s, want 1", affected, policyID)
			}
		}
	}
	return nil
}

func nestedBudgetUnit(quantityKind string) string {
	switch quantityKind {
	case "input-tokens", "output-tokens", "total-tokens":
		return "token"
	case "requests":
		return "request"
	case "wall-ms":
		return "millisecond"
	case "concurrency":
		return "slot"
	default:
		return "local-policy-unit"
	}
}

func renewNestedSchedulerReservationsTx(ctx context.Context, tx Tx, childRunID, executorID string, claimGeneration int64, now, leaseUntil string) error {
	expected, err := nestedSchedulerResourceHistoryCountTx(ctx, tx, childRunID, claimGeneration)
	if err != nil {
		return fmt.Errorf("renew nested scheduler reservations: %w", err)
	}
	budgetHistory, err := nestedSchedulerBudgetHistoryCountTx(ctx, tx, childRunID, claimGeneration)
	if err != nil {
		return fmt.Errorf("renew nested scheduler reservations: %w", err)
	}
	if expected == 0 {
		if budgetHistory > 0 {
			return federationError(ErrChildBudgetRequiredCode, "missing concurrency reservation authority for %s generation %d", childRunID, claimGeneration)
		}
		return nil
	}
	if budgetHistory > 0 && expected != 3 {
		return federationError(ErrChildBudgetRequiredCode, "found %d concurrency reservation authority rows for %s generation %d, want 3", expected, childRunID, claimGeneration)
	}
	result, err := tx.Exec(ctx, `UPDATE nested_scheduler_resource_reservations
		SET heartbeat_at = ?, lease_expires_at = ?, updated_at = ?
		WHERE run_id = ? AND executor_id = ? AND claim_generation = ? AND state = ? AND lease_expires_at > ?`,
		now, leaseUntil, now, childRunID, executorID, claimGeneration, schedulerReservationStateActive, now)
	if err != nil {
		return fmt.Errorf("renew nested scheduler reservations: %w", err)
	}
	affected, err := result.RowsAffected()
	if err == nil && affected != expected {
		return federationError(ErrChildBudgetRequiredCode, "renewed %d concurrency reservation rows for %s generation %d, want %d", affected, childRunID, claimGeneration, expected)
	}
	return nil
}

func releaseNestedSchedulerReservationsTx(ctx context.Context, tx Tx, childRunID, executorID string, claimGeneration int64, at string) error {
	expected, err := nestedSchedulerResourceHistoryCountTx(ctx, tx, childRunID, claimGeneration)
	if err != nil {
		return fmt.Errorf("release nested scheduler reservations: %w", err)
	}
	budgetHistory, err := nestedSchedulerBudgetHistoryCountTx(ctx, tx, childRunID, claimGeneration)
	if err != nil {
		return fmt.Errorf("release nested scheduler reservations: %w", err)
	}
	if expected == 0 {
		if budgetHistory > 0 {
			return federationError(ErrChildBudgetRequiredCode, "missing concurrency reservation authority for %s generation %d", childRunID, claimGeneration)
		}
		return nil
	}
	if budgetHistory > 0 && expected != 3 {
		return federationError(ErrChildBudgetRequiredCode, "found %d concurrency reservation authority rows for %s generation %d, want 3", expected, childRunID, claimGeneration)
	}
	result, err := tx.Exec(ctx, `UPDATE nested_scheduler_resource_reservations
		SET state = ?, heartbeat_at = ?, updated_at = ?
		WHERE run_id = ? AND executor_id = ? AND claim_generation = ? AND state = ? AND lease_expires_at > ?`,
		schedulerReservationStateReleased, at, at, childRunID, executorID, claimGeneration, schedulerReservationStateActive, at)
	if err != nil {
		return fmt.Errorf("release nested scheduler reservations: %w", err)
	}
	affected, err := result.RowsAffected()
	if err == nil && affected != expected && budgetHistory > 0 {
		return federationError(ErrChildBudgetRequiredCode, "released %d concurrency reservation rows for %s generation %d, want %d", affected, childRunID, claimGeneration, expected)
	}
	return nil
}

func claimLeaseActive(leaseExpiresAt, now string) bool {
	leaseExpiresAt = strings.TrimSpace(leaseExpiresAt)
	now = strings.TrimSpace(now)
	if leaseExpiresAt == "" || now == "" {
		return false
	}
	lease, leaseErr := time.Parse(time.RFC3339Nano, leaseExpiresAt)
	current, nowErr := time.Parse(time.RFC3339Nano, now)
	if leaseErr == nil && nowErr == nil {
		return lease.After(current)
	}
	return leaseExpiresAt > now
}

func durableTerminalStatus(status string) bool {
	switch normalizeDurableStatus(status) {
	case "succeeded", "succeeded_with_optional_failures", "failed", "cancelled", "timed_out", "abandoned", "needs-human", "skipped", "hung", "idle", "blocked":
		return true
	default:
		return false
	}
}

func durableBlockedStatus(status string) bool {
	switch normalizeDurableStatus(status) {
	case "needs-human", "abandoned", "skipped", "blocked":
		return true
	default:
		return false
	}
}

func appendClaimPhaseEvent(ctx context.Context, tx Tx, runID, at, phase, source string) error {
	payload, err := json.Marshal(map[string]string{
		"run_id": runID,
		"phase":  phase,
		"source": source,
	})
	if err != nil {
		return fmt.Errorf("marshal run claim phase event: %w", err)
	}
	return appendRunTransitionEvent(ctx, tx, runID, at, "run.claim.phase", string(payload))
}

func appendRunTransitionEvent(ctx context.Context, tx Tx, runID, at, eventType, payloadJSON string) error {
	var sequence int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM run_events WHERE run_id = ?`, runID).Scan(&sequence); err != nil {
		return fmt.Errorf("append run transition event: %w", err)
	}
	id := fmt.Sprintf("%s:%06d", runID, sequence)
	if _, err := tx.Exec(ctx, `INSERT INTO run_events(id, run_id, sequence, ts, event_type, payload_json) VALUES (?, ?, ?, ?, ?, ?)`,
		id, runID, sequence, at, eventType, payloadJSON); err != nil {
		return fmt.Errorf("append run transition event: %w", err)
	}
	return nil
}

var durableAllowedTransitions = map[string][]string{
	"planned":                          {"queued", "launching", "running", "waiting", "cancelled", "timed_out", "abandoned", "needs-human", "skipped"},
	"queued":                           {"launching", "running", "waiting", "cancelled", "timed_out", "abandoned", "needs-human", "skipped"},
	"launching":                        {"running", "waiting", "finishing", "succeeded", "succeeded_with_optional_failures", "failed", "cancelled", "timed_out", "abandoned", "needs-human", "skipped", "hung", "idle", "blocked"},
	"running":                          {"waiting", "finishing", "succeeded", "succeeded_with_optional_failures", "failed", "cancelled", "timed_out", "abandoned", "needs-human", "skipped", "hung", "idle", "blocked"},
	"waiting":                          {"queued", "launching", "running", "finishing", "succeeded", "succeeded_with_optional_failures", "failed", "cancelled", "timed_out", "abandoned", "needs-human", "skipped", "hung", "idle", "blocked"},
	"finishing":                        {"succeeded", "succeeded_with_optional_failures", "failed", "cancelled", "timed_out", "abandoned", "needs-human", "skipped", "hung", "idle", "blocked"},
	"succeeded":                        nil,
	"succeeded_with_optional_failures": nil,
	"failed":                           {"queued", "launching", "running", "waiting", "needs-human", "abandoned"},
	"cancelled":                        {"queued", "launching", "running", "waiting", "needs-human", "abandoned"},
	"timed_out":                        {"queued", "launching", "running", "waiting", "needs-human", "abandoned"},
	"abandoned":                        nil,
	"needs-human":                      nil,
	"skipped":                          nil,
	"hung":                             {"queued", "launching", "running", "waiting", "needs-human", "abandoned"},
	"idle":                             {"queued", "launching", "running", "waiting", "needs-human", "abandoned"},
	"blocked":                          {"queued", "launching", "running", "waiting", "needs-human", "abandoned"},
	"pending":                          {"queued", "launching", "running", "waiting", "cancelled", "timed_out", "abandoned", "needs-human", "skipped"},
}

func validateDurableTransition(from, to string) error {
	from = normalizeDurableStatus(from)
	to = normalizeDurableStatus(to)
	if !validDurableStatus(to) {
		return fmt.Errorf("invalid durable status %q", to)
	}
	if from == "" || from == to {
		return nil
	}
	if !validDurableStatus(from) {
		return fmt.Errorf("invalid previous durable status %q", from)
	}
	for _, allowed := range durableAllowedTransitions[from] {
		if allowed == to {
			return nil
		}
	}
	return fmt.Errorf("invalid durable transition %s -> %s", from, to)
}

func validDurableStatus(status string) bool {
	_, ok := durableAllowedTransitions[normalizeDurableStatus(status)]
	return ok
}

func normalizeDurableStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "success", "completed", "complete", "done":
		return "succeeded"
	case "succeeded-with-optional-failures", "succeeded with optional failures":
		return "succeeded_with_optional_failures"
	case "failure", "error":
		return "failed"
	case "canceled":
		return "cancelled"
	case "timeout", "timed-out":
		return "timed_out"
	case "needs_human", "needs human":
		return "needs-human"
	case "interrupted":
		return "running"
	default:
		return strings.ToLower(strings.TrimSpace(status))
	}
}

func validateExistingRunCompatible(kind string, desired RunNode, existing storedRunNode) error {
	desiredParent := strings.TrimSpace(desired.ParentRunID)
	if (kind == "child" || desiredParent != "") && strings.TrimSpace(existing.ParentRunID) != desiredParent {
		return fmt.Errorf("persist child plan graph: existing %s %q parent mismatch: %q != %q", kind, desired.RunID, existing.ParentRunID, desired.ParentRunID)
	}
	if strings.TrimSpace(existing.RootRunID) != "" && strings.TrimSpace(existing.RootRunID) != strings.TrimSpace(desired.RootRunID) {
		return fmt.Errorf("persist child plan graph: existing %s %q root mismatch: %q != %q", kind, desired.RunID, existing.RootRunID, desired.RootRunID)
	}
	if existing.Depth != desired.Depth {
		return fmt.Errorf("persist child plan graph: existing %s %q depth mismatch: %d != %d", kind, desired.RunID, existing.Depth, desired.Depth)
	}
	return nil
}

func runAncestors(ctx context.Context, tx Tx, runID string) (map[string]bool, error) {
	return recursiveRunSet(ctx, tx, `WITH RECURSIVE ancestors(id) AS (
		SELECT parent_run_id FROM runs WHERE id = ? AND parent_run_id IS NOT NULL AND parent_run_id <> ''
		UNION
		SELECT runs.parent_run_id FROM runs JOIN ancestors ON runs.id = ancestors.id WHERE runs.parent_run_id IS NOT NULL AND runs.parent_run_id <> ''
	) SELECT id FROM ancestors`, runID)
}

func runDescendants(ctx context.Context, tx Tx, runID string) (map[string]bool, error) {
	return recursiveRunSet(ctx, tx, `WITH RECURSIVE descendants(id) AS (
		SELECT child_run_id FROM run_edges WHERE parent_run_id = ?
		UNION
		SELECT run_edges.child_run_id FROM run_edges JOIN descendants ON run_edges.parent_run_id = descendants.id
	) SELECT id FROM descendants`, runID)
}

func recursiveRunSet(ctx context.Context, tx Tx, query string, args ...any) (map[string]bool, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("inspect durable run graph: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("inspect durable run graph: %w", err)
		}
		if strings.TrimSpace(id) != "" {
			out[strings.TrimSpace(id)] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("inspect durable run graph: %w", err)
	}
	return out, nil
}

func stringSetContains(values map[string]bool, value string) bool {
	return values[strings.TrimSpace(value)]
}

func firstNonEmptyNestedGraph(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
