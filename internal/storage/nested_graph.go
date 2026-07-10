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
	PlanID          string
	ParentRunID     string
	RootRunID       string
	SchemaVersion   string
	MaxDepth        int
	MaxConcurrency  int
	PlanJSON        string
	PlanFingerprint string
	CreatedAt       string
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

// ChildPlanSnapshot is the durable SQLite view of one accepted child plan.
type ChildPlanSnapshot struct {
	Plan  ChildPlanRecord
	Edges []RunEdgeSnapshot
}

// RunEdgeSnapshot is one persisted child edge and the current child run state.
type RunEdgeSnapshot struct {
	RunEdgeRecord
	RunStatus string
	StartedAt string
	EndedAt   string
}

// LoadChildPlanSnapshot returns the authoritative child-plan graph for planID.
func LoadChildPlanSnapshot(ctx context.Context, store Store, planID string) (ChildPlanSnapshot, bool, error) {
	if store == nil {
		return ChildPlanSnapshot{}, false, nil
	}
	planID = strings.TrimSpace(planID)
	if planID == "" {
		return ChildPlanSnapshot{}, false, fmt.Errorf("load child plan snapshot: plan_id is required")
	}
	var snapshot ChildPlanSnapshot
	found := false
	err := store.WithTx(ctx, func(tx Tx) error {
		var fingerprint string
		err := tx.QueryRow(ctx, `SELECT plan_id, parent_run_id, root_run_id, schema_version, max_depth, max_concurrency, plan_json, plan_fingerprint, created_at
			FROM child_plans WHERE plan_id = ?`, planID).Scan(
			&snapshot.Plan.PlanID,
			&snapshot.Plan.ParentRunID,
			&snapshot.Plan.RootRunID,
			&snapshot.Plan.SchemaVersion,
			&snapshot.Plan.MaxDepth,
			&snapshot.Plan.MaxConcurrency,
			&snapshot.Plan.PlanJSON,
			&fingerprint,
			&snapshot.Plan.CreatedAt,
		)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return fmt.Errorf("load child plan %q: %w", planID, err)
		}
		found = true
		snapshot.Plan.PlanFingerprint = strings.TrimSpace(fingerprint)

		rows, err := tx.Query(ctx, `SELECT
				e.parent_run_id, e.child_run_id, e.root_run_id, e.plan_id, e.child_key, e.depth, e.ordinal,
				e.scope_json, e.permission, e.aggregation_json, e.status, e.created_at, e.updated_at,
				r.status, COALESCE(r.started_at, ''), COALESCE(r.ended_at, '')
			FROM run_edges e
			JOIN runs r ON r.id = e.child_run_id
			WHERE e.plan_id = ?
			ORDER BY e.ordinal, e.child_key, e.child_run_id`, planID)
		if err != nil {
			return fmt.Errorf("load child plan %q edges: %w", planID, err)
		}
		defer rows.Close()
		for rows.Next() {
			var edge RunEdgeSnapshot
			if err := rows.Scan(
				&edge.ParentRunID,
				&edge.ChildRunID,
				&edge.RootRunID,
				&edge.PlanID,
				&edge.ChildKey,
				&edge.Depth,
				&edge.Ordinal,
				&edge.ScopeJSON,
				&edge.Permission,
				&edge.AggregationJSON,
				&edge.Status,
				&edge.CreatedAt,
				&edge.UpdatedAt,
				&edge.RunStatus,
				&edge.StartedAt,
				&edge.EndedAt,
			); err != nil {
				return fmt.Errorf("load child plan %q edge: %w", planID, err)
			}
			snapshot.Edges = append(snapshot.Edges, edge)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("load child plan %q edges: %w", planID, err)
		}
		return nil
	})
	if err != nil {
		return ChildPlanSnapshot{}, false, err
	}
	return snapshot, found, nil
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
		if err := persistChildPlanRecord(ctx, tx, plan); err != nil {
			return err
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

func persistChildPlanRecord(ctx context.Context, tx Tx, plan ChildPlanRecord) error {
	existing, ok, err := lookupChildPlanRecord(ctx, tx, plan.PlanID)
	if err != nil {
		return err
	}
	if ok {
		if err := validateExistingChildPlanRecord(plan, existing); err != nil {
			return err
		}
		if existing.PlanFingerprint == "" && strings.TrimSpace(plan.PlanFingerprint) != "" {
			if _, err := tx.Exec(ctx, `UPDATE child_plans SET plan_fingerprint = ? WHERE plan_id = ?`, plan.PlanFingerprint, plan.PlanID); err != nil {
				return fmt.Errorf("persist child plan %s fingerprint: %w", plan.PlanID, err)
			}
		}
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO child_plans(plan_id, parent_run_id, root_run_id, schema_version, max_depth, max_concurrency, plan_json, plan_fingerprint, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		plan.PlanID, plan.ParentRunID, plan.RootRunID, plan.SchemaVersion, plan.MaxDepth, plan.MaxConcurrency, plan.PlanJSON, plan.PlanFingerprint, plan.CreatedAt); err != nil {
		return fmt.Errorf("persist child plan %s: %w", plan.PlanID, err)
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
			status = CASE
				WHEN runs.status IN ('succeeded', 'failed', 'cancelled', 'timed_out', 'abandoned', 'needs-human') THEN runs.status
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
				WHEN run_edges.status IN ('succeeded', 'failed', 'cancelled', 'timed_out', 'abandoned', 'needs-human') THEN run_edges.status
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

func lookupChildPlanRecord(ctx context.Context, tx Tx, planID string) (ChildPlanRecord, bool, error) {
	var plan ChildPlanRecord
	err := tx.QueryRow(ctx, `SELECT plan_id, parent_run_id, root_run_id, schema_version, max_depth, max_concurrency, plan_json, plan_fingerprint, created_at
		FROM child_plans WHERE plan_id = ?`, planID).Scan(
		&plan.PlanID,
		&plan.ParentRunID,
		&plan.RootRunID,
		&plan.SchemaVersion,
		&plan.MaxDepth,
		&plan.MaxConcurrency,
		&plan.PlanJSON,
		&plan.PlanFingerprint,
		&plan.CreatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return ChildPlanRecord{}, false, nil
		}
		return ChildPlanRecord{}, false, fmt.Errorf("inspect child plan %q: %w", planID, err)
	}
	return plan, true, nil
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

func validateExistingChildPlanRecord(desired, existing ChildPlanRecord) error {
	checks := []struct {
		field string
		want  string
		got   string
	}{
		{"parent_run_id", desired.ParentRunID, existing.ParentRunID},
		{"root_run_id", desired.RootRunID, existing.RootRunID},
		{"schema_version", desired.SchemaVersion, existing.SchemaVersion},
	}
	for _, check := range checks {
		if strings.TrimSpace(check.want) != strings.TrimSpace(check.got) {
			return fmt.Errorf("persist child plan graph: plan_id %q already exists with different %s: %q != %q", desired.PlanID, check.field, check.got, check.want)
		}
	}
	if desired.MaxDepth != existing.MaxDepth {
		return fmt.Errorf("persist child plan graph: plan_id %q already exists with different max_depth: %d != %d", desired.PlanID, existing.MaxDepth, desired.MaxDepth)
	}
	if desired.MaxConcurrency != existing.MaxConcurrency {
		return fmt.Errorf("persist child plan graph: plan_id %q already exists with different max_concurrency: %d != %d", desired.PlanID, existing.MaxConcurrency, desired.MaxConcurrency)
	}
	if strings.TrimSpace(existing.PlanFingerprint) != "" && strings.TrimSpace(desired.PlanFingerprint) != "" && existing.PlanFingerprint != desired.PlanFingerprint {
		return fmt.Errorf("persist child plan graph: plan_id %q already exists with different plan_fingerprint; use a new plan_id for revised child plans", desired.PlanID)
	}
	return nil
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
