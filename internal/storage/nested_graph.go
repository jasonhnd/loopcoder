package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
	return store.WithTx(ctx, func(tx Tx) error {
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

// UpdateChildRunOutcome records a child edge and child run terminal status in
// one transaction.
func UpdateChildRunOutcome(ctx context.Context, store Store, parentRunID, childRunID, status, updatedAt string) error {
	if store == nil {
		return nil
	}
	parentRunID = strings.TrimSpace(parentRunID)
	childRunID = strings.TrimSpace(childRunID)
	status = strings.TrimSpace(status)
	updatedAt = strings.TrimSpace(updatedAt)
	if parentRunID == "" || childRunID == "" || status == "" || updatedAt == "" {
		return fmt.Errorf("update child run outcome: parent_run_id, child_run_id, status, and updated_at are required")
	}
	return store.WithTx(ctx, func(tx Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE run_edges SET status = ?, updated_at = ? WHERE parent_run_id = ? AND child_run_id = ?`,
			status, updatedAt, parentRunID, childRunID); err != nil {
			return fmt.Errorf("update run edge outcome: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE runs SET status = ?, ended_at = CASE WHEN ? IN ('succeeded', 'failed', 'cancelled', 'timed_out', 'abandoned', 'needs-human') THEN ? ELSE ended_at END, updated_at = ? WHERE id = ?`,
			status, status, updatedAt, updatedAt, childRunID); err != nil {
			return fmt.Errorf("update child run outcome: %w", err)
		}
		return nil
	})
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
			status = CASE WHEN excluded.status <> '' THEN excluded.status ELSE runs.status END,
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
			status = excluded.status,
			updated_at = excluded.updated_at`,
		edge.ParentRunID, edge.ChildRunID, edge.CreatedAt, edge.RootRunID, edge.PlanID, edge.ChildKey, edge.Depth, edge.Ordinal, edge.ScopeJSON, edge.Permission, edge.AggregationJSON, edge.Status, edge.UpdatedAt)
	if err != nil {
		return fmt.Errorf("persist run edge %s/%s: %w", edge.ParentRunID, edge.ChildRunID, err)
	}
	return nil
}
