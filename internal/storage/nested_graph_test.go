package storage

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestPersistChildPlanGraphRejectsSelfCycleWithoutPartialRows(t *testing.T) {
	ctx := context.Background()
	store := openNestedGraphTestStore(t, ctx)
	parent := nestedGraphRun("run-20260710T120000Z-wave", "", "run-20260710T120000Z-wave", 0)
	child := nestedGraphRun(parent.RunID, parent.RunID, parent.RootRunID, 1)
	edge := nestedGraphEdge(parent.RunID, child.RunID, parent.RootRunID, 1, 0, "self")

	err := PersistChildPlanGraph(ctx, store, parent, []RunNode{child}, nestedGraphPlan(parent.RunID, parent.RootRunID), []RunEdgeRecord{edge})
	if err == nil || !strings.Contains(err.Error(), "reuses parent, root, or ancestor") {
		t.Fatalf("PersistChildPlanGraph error = %v, want self-cycle rejection", err)
	}
	assertNestedGraphCounts(t, ctx, store, 0, 0, 0)
}

func TestPersistChildPlanGraphRejectsMultiLevelCycle(t *testing.T) {
	ctx := context.Background()
	store := openNestedGraphTestStore(t, ctx)
	root := nestedGraphRun("run-20260710T120000Z-wave", "", "run-20260710T120000Z-wave", 0)
	parent := nestedGraphRun("run-20260710T120001Z-child-0-parent", root.RunID, root.RootRunID, 1)
	if err := PersistChildPlanGraph(ctx, store, root, []RunNode{parent}, nestedGraphPlan(root.RunID, root.RootRunID), []RunEdgeRecord{
		nestedGraphEdge(root.RunID, parent.RunID, root.RootRunID, 1, 0, "parent"),
	}); err != nil {
		t.Fatalf("seed graph: %v", err)
	}

	err := PersistChildPlanGraph(ctx, store, parent, []RunNode{
		nestedGraphRun(root.RunID, parent.RunID, root.RootRunID, 2),
	}, nestedGraphPlan(parent.RunID, root.RootRunID), []RunEdgeRecord{
		nestedGraphEdge(parent.RunID, root.RunID, root.RootRunID, 2, 0, "cycle"),
	})
	if err == nil || !strings.Contains(err.Error(), "reuses parent, root, or ancestor") {
		t.Fatalf("PersistChildPlanGraph error = %v, want ancestor cycle rejection", err)
	}
	assertNestedGraphCounts(t, ctx, store, 2, 1, 1)
}

func TestPersistChildPlanGraphRejectsRootDepthOrdinalAndExistingIDMismatches(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(parent *RunNode, child *RunNode, plan *ChildPlanRecord, edge *RunEdgeRecord, store Store, ctx context.Context, t *testing.T)
		wantErr string
	}{
		{
			name: "plan root mismatch",
			mutate: func(_ *RunNode, _ *RunNode, plan *ChildPlanRecord, _ *RunEdgeRecord, _ Store, _ context.Context, _ *testing.T) {
				plan.RootRunID = "run-20260710T120099Z-wave"
			},
			wantErr: "does not match plan root",
		},
		{
			name: "child depth mismatch",
			mutate: func(_ *RunNode, child *RunNode, _ *ChildPlanRecord, edge *RunEdgeRecord, _ Store, _ context.Context, _ *testing.T) {
				child.Depth = 3
				edge.Depth = 3
			},
			wantErr: "does not match parent depth",
		},
		{
			name: "edge depth mismatch",
			mutate: func(_ *RunNode, _ *RunNode, _ *ChildPlanRecord, edge *RunEdgeRecord, _ Store, _ context.Context, _ *testing.T) {
				edge.Depth = 7
			},
			wantErr: "does not match child depth",
		},
		{
			name: "ordinal mismatch",
			mutate: func(_ *RunNode, _ *RunNode, _ *ChildPlanRecord, edge *RunEdgeRecord, _ Store, _ context.Context, _ *testing.T) {
				edge.Ordinal = 2
			},
			wantErr: "does not match item index",
		},
		{
			name: "existing child belongs to another parent",
			mutate: func(_ *RunNode, child *RunNode, _ *ChildPlanRecord, _ *RunEdgeRecord, store Store, ctx context.Context, t *testing.T) {
				if err := store.WithTx(ctx, func(tx Tx) error {
					_, err := tx.Exec(ctx, `INSERT INTO runs(id, parent_run_id, root_run_id, depth, status, updated_at, created_at) VALUES (?, ?, ?, ?, 'queued', '2026-07-10T12:00:00Z', '2026-07-10T12:00:00Z')`,
						child.RunID, "run-20260710T120050Z-wave", "run-20260710T120050Z-wave", child.Depth)
					return err
				}); err != nil {
					t.Fatalf("seed incompatible child: %v", err)
				}
			},
			wantErr: "already belongs to parent",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openNestedGraphTestStore(t, ctx)
			parent := nestedGraphRun("run-20260710T120000Z-wave", "", "run-20260710T120000Z-wave", 0)
			child := nestedGraphRun("run-20260710T120001Z-child-0-alpha", parent.RunID, parent.RootRunID, 1)
			plan := nestedGraphPlan(parent.RunID, parent.RootRunID)
			edge := nestedGraphEdge(parent.RunID, child.RunID, parent.RootRunID, child.Depth, 0, "alpha")
			tc.mutate(&parent, &child, &plan, &edge, store, ctx, t)

			err := PersistChildPlanGraph(ctx, store, parent, []RunNode{child}, plan, []RunEdgeRecord{edge})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("PersistChildPlanGraph error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestCheckHealthSurfacesDurableGraphSelfEdge(t *testing.T) {
	ctx := context.Background()
	store := openNestedGraphTestStore(t, ctx)
	path := store.Path()
	if err := store.WithTx(ctx, func(tx Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO runs(id, parent_run_id, root_run_id, depth, status, updated_at, created_at) VALUES ('run-20260710T120000Z-wave', 'run-20260710T120000Z-wave', 'run-20260710T120000Z-wave', 0, 'succeeded', '2026-07-10T12:00:00Z', '2026-07-10T12:00:00Z')`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO run_edges(parent_run_id, child_run_id, edge_type, created_at, root_run_id, plan_id, child_key, depth, ordinal, scope_json, permission, aggregation_json, status, updated_at) VALUES ('run-20260710T120000Z-wave', 'run-20260710T120000Z-wave', 'child', '2026-07-10T12:00:00Z', 'run-20260710T120000Z-wave', 'plan-corrupt', 'self', 0, 0, '{}', 'write', '{}', 'succeeded', '2026-07-10T12:00:00Z')`)
		return err
	}); err != nil {
		t.Fatalf("seed corrupt graph: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	_, err := CheckHealth(ctx, path)
	if err == nil || !strings.Contains(err.Error(), "durable graph: self edge") {
		t.Fatalf("CheckHealth error = %v, want durable graph self-edge diagnostic", err)
	}
}

func openNestedGraphTestStore(t *testing.T, ctx context.Context) Store {
	t.Helper()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func nestedGraphRun(id, parent, root string, depth int) RunNode {
	return RunNode{
		RunID:       id,
		ParentRunID: parent,
		RootRunID:   root,
		Depth:       depth,
		Origin:      "nested_test",
		Status:      "queued",
		CreatedAt:   "2026-07-10T12:00:00Z",
		UpdatedAt:   "2026-07-10T12:00:00Z",
	}
}

func nestedGraphPlan(parent, root string) ChildPlanRecord {
	return ChildPlanRecord{
		PlanID:         "plan-" + parent,
		ParentRunID:    parent,
		RootRunID:      root,
		SchemaVersion:  "loopcoder.child_plan.v1",
		MaxDepth:       4,
		MaxConcurrency: 1,
		PlanJSON:       "{}",
		CreatedAt:      "2026-07-10T12:00:00Z",
	}
}

func nestedGraphEdge(parent, child, root string, depth, ordinal int, key string) RunEdgeRecord {
	return RunEdgeRecord{
		ParentRunID:     parent,
		ChildRunID:      child,
		RootRunID:       root,
		PlanID:          "plan-" + parent,
		ChildKey:        key,
		Depth:           depth,
		Ordinal:         ordinal,
		ScopeJSON:       "{}",
		Permission:      "write",
		AggregationJSON: "{}",
		Status:          "queued",
		CreatedAt:       "2026-07-10T12:00:00Z",
		UpdatedAt:       "2026-07-10T12:00:00Z",
	}
}

func assertNestedGraphCounts(t *testing.T, ctx context.Context, store Store, wantRuns, wantEdges, wantPlans int) {
	t.Helper()
	var runs, edges, plans int
	if err := store.WithTx(ctx, func(tx Tx) error {
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM runs`).Scan(&runs); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM run_edges`).Scan(&edges); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM child_plans`).Scan(&plans)
	}); err != nil {
		t.Fatalf("query graph counts: %v", err)
	}
	if runs != wantRuns || edges != wantEdges || plans != wantPlans {
		t.Fatalf("graph counts runs/edges/plans = %d/%d/%d, want %d/%d/%d", runs, edges, plans, wantRuns, wantEdges, wantPlans)
	}
}
