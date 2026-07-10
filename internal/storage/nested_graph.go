package storage

import (
	"context"
	"database/sql"
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
	edge.ParentRunID = strings.TrimSpace(edge.ParentRunID)
	edge.ChildRunID = strings.TrimSpace(edge.ChildRunID)
	if edge.ParentRunID == "" || edge.ChildRunID == "" {
		return fmt.Errorf("persist run edge: parent_run_id and child_run_id are required")
	}
	if edge.ParentRunID == edge.ChildRunID {
		return fmt.Errorf("persist run edge %s/%s: self edge is not allowed", edge.ParentRunID, edge.ChildRunID)
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

func validateChildPlanGraph(ctx context.Context, tx Tx, parent RunNode, children []RunNode, plan ChildPlanRecord, edges []RunEdgeRecord) error {
	parent.RunID = strings.TrimSpace(parent.RunID)
	parent.RootRunID = firstGraphValue(parent.RootRunID, parent.RunID)
	plan.ParentRunID = strings.TrimSpace(plan.ParentRunID)
	plan.RootRunID = strings.TrimSpace(plan.RootRunID)
	if parent.RunID == "" || plan.ParentRunID == "" || plan.RootRunID == "" {
		return fmt.Errorf("persist child plan graph: parent_run_id and root_run_id are required")
	}
	if parent.RunID != plan.ParentRunID {
		return fmt.Errorf("persist child plan graph: parent node %q does not match plan parent %q", parent.RunID, plan.ParentRunID)
	}
	if parent.RootRunID != plan.RootRunID {
		return fmt.Errorf("persist child plan graph: parent root %q does not match plan root %q", parent.RootRunID, plan.RootRunID)
	}
	if parent.Depth < 0 {
		return fmt.Errorf("persist child plan graph: parent depth must be non-negative")
	}
	if parent.Depth == 0 && parent.RunID != parent.RootRunID {
		return fmt.Errorf("persist child plan graph: depth-0 parent %q must be its own root, not %q", parent.RunID, parent.RootRunID)
	}
	if err := validateExistingRunCompatibility(ctx, tx, parent, true); err != nil {
		return err
	}
	ancestors, err := durableAncestors(ctx, tx, parent.RunID)
	if err != nil {
		return err
	}
	ancestors[parent.RunID] = true
	ancestors[parent.RootRunID] = true
	seenChildIDs := map[string]bool{}
	seenOrdinals := map[int]bool{}
	seenKeys := map[string]bool{}
	for i, child := range children {
		child.RunID = strings.TrimSpace(child.RunID)
		child.ParentRunID = strings.TrimSpace(child.ParentRunID)
		child.RootRunID = strings.TrimSpace(child.RootRunID)
		if child.RunID == "" {
			return fmt.Errorf("persist child plan graph: child[%d] run_id is required", i)
		}
		if ancestors[child.RunID] {
			return fmt.Errorf("persist child plan graph: child run %q reuses parent, root, or ancestor run id", child.RunID)
		}
		if seenChildIDs[child.RunID] {
			return fmt.Errorf("persist child plan graph: duplicate child run id %q", child.RunID)
		}
		seenChildIDs[child.RunID] = true
		if child.ParentRunID != parent.RunID {
			return fmt.Errorf("persist child plan graph: child %q parent %q does not match %q", child.RunID, child.ParentRunID, parent.RunID)
		}
		if child.RootRunID != parent.RootRunID {
			return fmt.Errorf("persist child plan graph: child %q root %q does not match %q", child.RunID, child.RootRunID, parent.RootRunID)
		}
		if child.Depth != parent.Depth+1 {
			return fmt.Errorf("persist child plan graph: child %q depth %d does not match parent depth %d", child.RunID, child.Depth, parent.Depth)
		}
		if err := validateExistingRunCompatibility(ctx, tx, child, false); err != nil {
			return err
		}
		if reachesRun(ctx, tx, child.RunID, parent.RunID, map[string]bool{}) {
			return fmt.Errorf("persist child plan graph: edge %s -> %s would create a cycle", parent.RunID, child.RunID)
		}
		edge := edges[i]
		edge.ParentRunID = strings.TrimSpace(edge.ParentRunID)
		edge.ChildRunID = strings.TrimSpace(edge.ChildRunID)
		edge.RootRunID = strings.TrimSpace(edge.RootRunID)
		edge.ChildKey = strings.TrimSpace(edge.ChildKey)
		if edge.ParentRunID != parent.RunID || edge.ChildRunID != child.RunID {
			return fmt.Errorf("persist child plan graph: edge[%d] does not match parent/child nodes", i)
		}
		if edge.ParentRunID == edge.ChildRunID {
			return fmt.Errorf("persist child plan graph: self edge %s is not allowed", edge.ParentRunID)
		}
		if edge.RootRunID != parent.RootRunID {
			return fmt.Errorf("persist child plan graph: edge %s/%s root %q does not match %q", edge.ParentRunID, edge.ChildRunID, edge.RootRunID, parent.RootRunID)
		}
		if edge.Depth != child.Depth {
			return fmt.Errorf("persist child plan graph: edge %s/%s depth %d does not match child depth %d", edge.ParentRunID, edge.ChildRunID, edge.Depth, child.Depth)
		}
		if edge.Ordinal != i {
			return fmt.Errorf("persist child plan graph: edge %s/%s ordinal %d does not match item index %d", edge.ParentRunID, edge.ChildRunID, edge.Ordinal, i)
		}
		if seenOrdinals[edge.Ordinal] {
			return fmt.Errorf("persist child plan graph: duplicate child ordinal %d", edge.Ordinal)
		}
		seenOrdinals[edge.Ordinal] = true
		if edge.ChildKey == "" {
			return fmt.Errorf("persist child plan graph: edge %s/%s child_key is required", edge.ParentRunID, edge.ChildRunID)
		}
		if seenKeys[edge.ChildKey] {
			return fmt.Errorf("persist child plan graph: duplicate child_key %q", edge.ChildKey)
		}
		seenKeys[edge.ChildKey] = true
	}
	return nil
}

func validateExistingRunCompatibility(ctx context.Context, tx Tx, run RunNode, parent bool) error {
	var existingParent sql.NullString
	var existingRoot sql.NullString
	var existingDepth int
	err := tx.QueryRow(ctx, `SELECT parent_run_id, root_run_id, depth FROM runs WHERE id = ?`, run.RunID).Scan(&existingParent, &existingRoot, &existingDepth)
	if err != nil {
		if err == sql.ErrNoRows {
			if parent && run.Depth > 0 {
				return fmt.Errorf("persist child plan graph: non-root parent run %q is missing from durable graph", run.RunID)
			}
			return nil
		}
		return fmt.Errorf("inspect existing run %s: %w", run.RunID, err)
	}
	if parent {
		if strings.TrimSpace(existingRoot.String) != "" && strings.TrimSpace(existingRoot.String) != run.RootRunID {
			return fmt.Errorf("persist child plan graph: existing parent run %q has root %q, not %q", run.RunID, existingRoot.String, run.RootRunID)
		}
		if existingDepth != run.Depth {
			return fmt.Errorf("persist child plan graph: existing parent run %q has depth %d, not %d", run.RunID, existingDepth, run.Depth)
		}
		return nil
	}
	if strings.TrimSpace(existingParent.String) != "" && strings.TrimSpace(existingParent.String) != run.ParentRunID {
		return fmt.Errorf("persist child plan graph: existing child run %q already belongs to parent %q", run.RunID, existingParent.String)
	}
	if strings.TrimSpace(existingRoot.String) != "" && strings.TrimSpace(existingRoot.String) != run.RootRunID {
		return fmt.Errorf("persist child plan graph: existing child run %q has root %q, not %q", run.RunID, existingRoot.String, run.RootRunID)
	}
	if existingDepth != run.Depth {
		return fmt.Errorf("persist child plan graph: existing child run %q has depth %d, not %d", run.RunID, existingDepth, run.Depth)
	}
	return nil
}

func durableAncestors(ctx context.Context, tx Tx, runID string) (map[string]bool, error) {
	ancestors := map[string]bool{}
	current := strings.TrimSpace(runID)
	for current != "" {
		if ancestors[current] {
			return ancestors, fmt.Errorf("persist child plan graph: existing durable graph has a cycle at %s", current)
		}
		ancestors[current] = true
		var parent sql.NullString
		err := tx.QueryRow(ctx, `SELECT parent_run_id FROM runs WHERE id = ?`, current).Scan(&parent)
		if err != nil {
			if err == sql.ErrNoRows {
				break
			}
			return ancestors, fmt.Errorf("inspect ancestors for %s: %w", runID, err)
		}
		current = strings.TrimSpace(parent.String)
	}
	return ancestors, nil
}

func reachesRun(ctx context.Context, tx Tx, fromRunID, targetRunID string, seen map[string]bool) bool {
	fromRunID = strings.TrimSpace(fromRunID)
	targetRunID = strings.TrimSpace(targetRunID)
	if fromRunID == "" || targetRunID == "" || seen[fromRunID] {
		return false
	}
	if fromRunID == targetRunID {
		return true
	}
	seen[fromRunID] = true
	rows, err := tx.Query(ctx, `SELECT child_run_id FROM run_edges WHERE parent_run_id = ?`, fromRunID)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var child string
		if err := rows.Scan(&child); err != nil {
			return false
		}
		if reachesRun(ctx, tx, child, targetRunID, seen) {
			return true
		}
	}
	return false
}

func firstGraphValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
