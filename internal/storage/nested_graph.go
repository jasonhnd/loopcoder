package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

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
	ClaimExecutorID string
	ClaimGeneration int
	ClaimedAt       string
	LeaseExpiresAt  string
	HeartbeatAt     string
}

type ClaimOutcome string

const (
	ClaimOutcomeClaimed        ClaimOutcome = "claimed"
	ClaimOutcomeAlreadyRunning ClaimOutcome = "already-running"
	ClaimOutcomeTerminalReused ClaimOutcome = "terminal-reused"
	ClaimOutcomeBlocked        ClaimOutcome = "blocked"
	ClaimOutcomeStaleClaim     ClaimOutcome = "stale-claim"
)

type ClaimResult struct {
	Outcome            ClaimOutcome
	PreviousOutcome    ClaimOutcome
	RunID              string
	ParentRunID        string
	ExecutorID         string
	ClaimGeneration    int
	ClaimedAt          string
	LeaseExpiresAt     string
	HeartbeatAt        string
	Status             string
	StartedAt          string
	FinishedAt         string
	PreviousExecutorID string
}

var ErrStaleClaimGeneration = fmt.Errorf("stale child run execution claim")

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
				COALESCE(c.heartbeat_at, '')
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
				&child.ClaimExecutorID,
				&child.ClaimGeneration,
				&child.ClaimedAt,
				&child.LeaseExpiresAt,
				&child.HeartbeatAt,
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

	return retryWriteTx(ctx, store, func(tx Tx) error {
		return applyRunStatusTransition(ctx, tx, transition, true)
	})
}

func applyRunStatusTransition(ctx context.Context, tx Tx, transition RunStatusTransition, appendEvent bool) error {
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
	if !appendEvent {
		return nil
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
	return appendRunTransitionEvent(ctx, tx, transition.RunID, transition.UpdatedAt, "run.status.transition", string(payload))
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
	return retryWriteTx(ctx, store, func(tx Tx) error {
		return persistChildPlanGraphTx(ctx, tx, parent, children, plan, edges)
	})
}

func persistChildPlanGraphTx(ctx context.Context, tx Tx, parent RunNode, children []RunNode, plan ChildPlanRecord, edges []RunEdgeRecord) error {
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

func ClaimChildRunExecution(ctx context.Context, store Store, parentRunID, childRunID, executorID string, now time.Time, leaseUntil time.Time) (ClaimResult, error) {
	result := ClaimResult{
		Outcome:     ClaimOutcomeClaimed,
		RunID:       strings.TrimSpace(childRunID),
		ParentRunID: strings.TrimSpace(parentRunID),
		ExecutorID:  strings.TrimSpace(executorID),
	}
	if store == nil {
		return result, nil
	}
	if result.ParentRunID == "" || result.RunID == "" || result.ExecutorID == "" {
		return ClaimResult{}, fmt.Errorf("claim child run execution: parent_run_id, child_run_id, and executor_id are required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if leaseUntil.IsZero() || !leaseUntil.After(now) {
		return ClaimResult{}, fmt.Errorf("claim child run execution: lease_until must be after now")
	}
	nowText := formatTimestamp(now)
	leaseText := formatTimestamp(leaseUntil)
	err := retryWriteTx(ctx, store, func(tx Tx) error {
		var status, startedAt, finishedAt string
		err := tx.QueryRow(ctx, `SELECT status, COALESCE(started_at, ''), COALESCE(ended_at, '') FROM runs WHERE id = ?`, result.RunID).Scan(&status, &startedAt, &finishedAt)
		if err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("claim child run execution: run %q is missing", result.RunID)
			}
			return fmt.Errorf("claim child run execution: inspect run %q: %w", result.RunID, err)
		}
		status = normalizeDurableStatus(status)
		result.Status = status
		result.StartedAt = startedAt
		result.FinishedAt = finishedAt
		var edgeStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM run_edges WHERE parent_run_id = ? AND child_run_id = ?`, result.ParentRunID, result.RunID).Scan(&edgeStatus); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("claim child run execution: edge %q/%q is missing", result.ParentRunID, result.RunID)
			}
			return fmt.Errorf("claim child run execution: inspect edge %q/%q: %w", result.ParentRunID, result.RunID, err)
		}
		edgeStatus = normalizeDurableStatus(edgeStatus)
		if terminalDurableStatus(status) || terminalDurableStatus(edgeStatus) {
			result.Outcome = ClaimOutcomeTerminalReused
			return nil
		}
		if blockedDurableStatus(status) || blockedDurableStatus(edgeStatus) {
			result.Outcome = ClaimOutcomeBlocked
			return nil
		}

		var claim claimRow
		hasClaim, err := loadClaimRow(ctx, tx, result.RunID, &claim)
		if err != nil {
			return err
		}
		if hasClaim {
			result.PreviousExecutorID = claim.ExecutorID
			if claim.LeaseExpiresAt != "" && claim.LeaseExpiresAt > nowText {
				result.Outcome = ClaimOutcomeAlreadyRunning
				result.ExecutorID = claim.ExecutorID
				result.ClaimGeneration = claim.Generation
				result.ClaimedAt = claim.ClaimedAt
				result.LeaseExpiresAt = claim.LeaseExpiresAt
				result.HeartbeatAt = claim.HeartbeatAt
				return nil
			}
			result.PreviousOutcome = ClaimOutcomeStaleClaim
			result.ClaimGeneration = claim.Generation + 1
		} else {
			result.ClaimGeneration = 1
		}
		result.Outcome = ClaimOutcomeClaimed
		result.ExecutorID = strings.TrimSpace(executorID)
		result.ClaimedAt = nowText
		result.LeaseExpiresAt = leaseText
		result.HeartbeatAt = nowText
		if _, err := tx.Exec(ctx, `INSERT INTO run_claims(run_id, executor_id, claim_generation, claimed_at, lease_expires_at, heartbeat_at)
				VALUES (?, ?, ?, ?, ?, ?)
				ON CONFLICT(run_id) DO UPDATE SET
					executor_id = excluded.executor_id,
					claim_generation = excluded.claim_generation,
					claimed_at = excluded.claimed_at,
					lease_expires_at = excluded.lease_expires_at,
					heartbeat_at = excluded.heartbeat_at`,
			result.RunID, result.ExecutorID, result.ClaimGeneration, result.ClaimedAt, result.LeaseExpiresAt, result.HeartbeatAt); err != nil {
			return fmt.Errorf("claim child run execution: persist claim: %w", err)
		}
		appendEvent := status != "running" || edgeStatus != "running" || result.PreviousOutcome == ClaimOutcomeStaleClaim
		transition := RunStatusTransition{
			RunID:       result.RunID,
			ParentRunID: result.ParentRunID,
			ChildRunID:  result.RunID,
			Status:      "running",
			UpdatedAt:   nowText,
			Reason:      "child execution claimed",
			Source:      "nested-scheduler",
		}
		if result.PreviousOutcome == ClaimOutcomeStaleClaim {
			transition.Reason = "stale child execution claim taken over"
		}
		if err := applyRunStatusTransition(ctx, tx, transition, appendEvent); err != nil {
			return err
		}
		result.Status = "running"
		if result.StartedAt == "" {
			result.StartedAt = nowText
		}
		return nil
	})
	return result, err
}

func CompleteClaimedChildRunExecution(ctx context.Context, store Store, parentRunID, childRunID string, claim ClaimResult, status, updatedAt, reason string) error {
	if store == nil {
		return nil
	}
	parentRunID = strings.TrimSpace(parentRunID)
	childRunID = strings.TrimSpace(childRunID)
	status = normalizeDurableStatus(status)
	updatedAt = strings.TrimSpace(updatedAt)
	if parentRunID == "" || childRunID == "" || strings.TrimSpace(claim.ExecutorID) == "" || claim.ClaimGeneration <= 0 || status == "" || updatedAt == "" {
		return fmt.Errorf("complete claimed child run execution: parent_run_id, child_run_id, executor claim, status, and updated_at are required")
	}
	return retryWriteTx(ctx, store, func(tx Tx) error {
		var currentExecutor string
		var currentGeneration int
		err := tx.QueryRow(ctx, `SELECT executor_id, claim_generation FROM run_claims WHERE run_id = ?`, childRunID).Scan(&currentExecutor, &currentGeneration)
		if err != nil {
			if err == sql.ErrNoRows {
				return ErrStaleClaimGeneration
			}
			return fmt.Errorf("complete claimed child run execution: inspect claim: %w", err)
		}
		if currentExecutor != claim.ExecutorID || currentGeneration != claim.ClaimGeneration {
			return ErrStaleClaimGeneration
		}
		if _, err := tx.Exec(ctx, `UPDATE run_claims SET heartbeat_at = ? WHERE run_id = ? AND executor_id = ? AND claim_generation = ?`,
			updatedAt, childRunID, claim.ExecutorID, claim.ClaimGeneration); err != nil {
			return fmt.Errorf("complete claimed child run execution: heartbeat claim: %w", err)
		}
		return applyRunStatusTransition(ctx, tx, RunStatusTransition{
			RunID:       childRunID,
			ParentRunID: parentRunID,
			ChildRunID:  childRunID,
			Status:      status,
			UpdatedAt:   updatedAt,
			Reason:      reason,
			Source:      "nested-scheduler",
		}, true)
	})
}

type claimRow struct {
	ExecutorID     string
	Generation     int
	ClaimedAt      string
	LeaseExpiresAt string
	HeartbeatAt    string
}

func loadClaimRow(ctx context.Context, tx Tx, runID string, out *claimRow) (bool, error) {
	err := tx.QueryRow(ctx, `SELECT executor_id, claim_generation, claimed_at, lease_expires_at, heartbeat_at FROM run_claims WHERE run_id = ?`, runID).Scan(
		&out.ExecutorID,
		&out.Generation,
		&out.ClaimedAt,
		&out.LeaseExpiresAt,
		&out.HeartbeatAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("inspect run claim %q: %w", runID, err)
	}
	return true, nil
}

func retryWriteTx(ctx context.Context, store Store, fn func(Tx) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		err = store.WithWriteTx(ctx, fn)
		if err == nil || !sqliteBusy(err) {
			return err
		}
		delay := time.Duration(attempt+1) * 25 * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return err
}

func terminalDurableStatus(status string) bool {
	switch normalizeDurableStatus(status) {
	case "succeeded", "succeeded_with_optional_failures", "failed", "cancelled", "timed_out", "abandoned", "skipped", "hung", "idle":
		return true
	default:
		return false
	}
}

func blockedDurableStatus(status string) bool {
	switch normalizeDurableStatus(status) {
	case "needs-human", "blocked":
		return true
	default:
		return false
	}
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
