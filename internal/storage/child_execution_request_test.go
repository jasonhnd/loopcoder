package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPersistChildPlanGraphWithExecutionRequestsRejectsFingerprintMismatch(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	parent, children, plan, edges, requests := childExecutionStorageFixture()
	if err := PersistChildPlanGraphWithExecutionRequests(ctx, store, parent, children, plan, edges, requests); err != nil {
		t.Fatalf("initial persist: %v", err)
	}
	if err := PersistChildPlanGraphWithExecutionRequests(ctx, store, parent, children, plan, edges, requests); err != nil {
		t.Fatalf("identical replay: %v", err)
	}
	mutated := append([]ChildExecutionRequestRecord(nil), requests...)
	mutated[0].RequestJSON = `{"schema_version":"test.v1","permission":"orchestrate"}`
	mutated[0].ContractFingerprint = "sha256:changed"
	err = PersistChildPlanGraphWithExecutionRequests(ctx, store, parent, children, plan, edges, mutated)
	if !errors.Is(err, ErrAgentFingerprintMismatch) {
		t.Fatalf("fingerprint replay error = %v, want ErrAgentFingerprintMismatch", err)
	}
	persisted, ok, err := LoadChildExecutionRequest(ctx, store, requests[0].ChildRunID)
	if err != nil || !ok {
		t.Fatalf("LoadChildExecutionRequest ok=%t error=%v", ok, err)
	}
	if persisted.ContractFingerprint != requests[0].ContractFingerprint || persisted.RequestJSON != requests[0].RequestJSON {
		t.Fatalf("rejected replay changed persisted contract: %#v", persisted)
	}
}

func TestChildExecutionRequestClaimLifecycleIsFenced(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 17, 2, 0, 0, 0, time.UTC)
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	parent, children, plan, edges, requests := childExecutionStorageFixture()
	if err := PersistChildPlanGraphWithExecutionRequests(ctx, store, parent, children, plan, edges, requests); err != nil {
		t.Fatalf("persist: %v", err)
	}
	claim, err := ClaimChildRunExecution(ctx, store, parent.RunID, children[0].RunID, "executor-contract", now, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ClaimChildRunExecution: %v", err)
	}
	bound, err := BindChildExecutionRequestClaim(ctx, store, children[0].RunID, claim.ExecutorID, claim.ClaimGeneration, requests[0].ContractFingerprint, ClaimPhaseClaimed, formatTimestamp(now))
	if err != nil {
		t.Fatalf("BindChildExecutionRequestClaim: %v", err)
	}
	if bound.ClaimGeneration != claim.ClaimGeneration || bound.Permission != requests[0].Permission {
		t.Fatalf("bound contract = %#v", bound)
	}
	if _, err := BindChildExecutionRequestClaim(ctx, store, children[0].RunID, "stale-executor", claim.ClaimGeneration, requests[0].ContractFingerprint, ClaimPhaseClaimed, formatTimestamp(now)); !IsStaleChildRunClaim(err) {
		t.Fatalf("stale bind error = %v, want stale claim", err)
	}
	if err := UpdateChildRunClaimPhase(ctx, store, parent.RunID, children[0].RunID, claim.ExecutorID, claim.ClaimGeneration, ClaimPhaseExecuting, formatTimestamp(now.Add(time.Second)), ""); err != nil {
		t.Fatalf("UpdateChildRunClaimPhase: %v", err)
	}
	if err := CompleteClaimedChildRun(ctx, store, parent.RunID, children[0].RunID, claim.ExecutorID, claim.ClaimGeneration, "succeeded", formatTimestamp(now.Add(2*time.Second)), "done", "receipt:test"); err != nil {
		t.Fatalf("CompleteClaimedChildRun: %v", err)
	}
	completed, ok, err := LoadChildExecutionRequest(ctx, store, children[0].RunID)
	if err != nil || !ok {
		t.Fatalf("LoadChildExecutionRequest completed ok=%t error=%v", ok, err)
	}
	if completed.ClaimGeneration != claim.ClaimGeneration || completed.LifecycleStatus != "succeeded" || completed.ContractFingerprint != requests[0].ContractFingerprint {
		t.Fatalf("completed contract = %#v", completed)
	}
}

func TestMigrationV31MarksLegacyChildExecutionRequestsNeedsHumanGolden(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	parent, children, plan, edges, _ := childExecutionStorageFixture()
	if err := PersistChildPlanGraph(ctx, store, parent, children, plan, edges); err != nil {
		t.Fatalf("PersistChildPlanGraph legacy fixture: %v", err)
	}
	if err := store.WithWriteTx(ctx, func(tx Tx) error {
		if _, err := tx.Exec(ctx, `DROP TABLE child_execution_requests`); err != nil {
			return err
		}
		// Drop tables introduced after v30 so reopen re-applies migrations 31+.
		for _, table := range []string{"workgraph_events", "workgraph_dependencies", "workgraph_items", "workgraph_versions"} {
			if _, err := tx.Exec(ctx, `DROP TABLE IF EXISTS `+table); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `DELETE FROM migrations WHERE version >= 31`)
		return err
	}); err != nil {
		t.Fatalf("rewind schema to v30: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close v30 fixture: %v", err)
	}
	store, err = Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("migrate v30 fixture: %v", err)
	}
	defer store.Close()
	record, ok, err := LoadChildExecutionRequest(ctx, store, children[0].RunID)
	if err != nil || !ok {
		t.Fatalf("LoadChildExecutionRequest migrated ok=%t error=%v", ok, err)
	}
	var runStatus, edgeStatus string
	if err := store.WithTx(ctx, func(tx Tx) error {
		if err := tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = ?`, children[0].RunID).Scan(&runStatus); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT status FROM run_edges WHERE parent_run_id = ? AND child_run_id = ?`, parent.RunID, children[0].RunID).Scan(&edgeStatus)
	}); err != nil {
		t.Fatalf("query migrated statuses: %v", err)
	}
	projection := struct {
		Record     ChildExecutionRequestRecord `json:"record"`
		RunStatus  string                      `json:"run_status"`
		EdgeStatus string                      `json:"edge_status"`
	}{Record: record, RunStatus: runStatus, EdgeStatus: edgeStatus}
	got, err := json.MarshalIndent(projection, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent migration projection: %v", err)
	}
	got = append(got, '\n')
	want, err := os.ReadFile(filepath.Join("testdata", "legacy_child_execution_request_v31.json"))
	if err != nil {
		t.Fatalf("read migration golden: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("legacy migration golden mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
	if !record.LegacyAmbiguous || record.LifecycleStatus != "needs-human" || runStatus != "needs-human" || edgeStatus != "needs-human" {
		t.Fatalf("legacy migration did not fail closed: record=%#v run=%q edge=%q", record, runStatus, edgeStatus)
	}
}

func childExecutionStorageFixture() (RunNode, []RunNode, ChildPlanRecord, []RunEdgeRecord, []ChildExecutionRequestRecord) {
	at := "2026-07-17T02:00:00Z"
	parent := RunNode{RunID: "run-contract-parent", RootRunID: "run-contract-parent", Depth: 0, Origin: "nested_parent", Status: "running", CreatedAt: at, UpdatedAt: at}
	children := []RunNode{{RunID: "run-contract-child", ParentRunID: parent.RunID, RootRunID: parent.RootRunID, Depth: 1, Origin: "sub_agent", Status: "queued", CreatedAt: at, UpdatedAt: at}}
	plan := ChildPlanRecord{PlanID: "plan-contract", ParentRunID: parent.RunID, RootRunID: parent.RootRunID, SchemaVersion: "loopcoder.child_plan.v1", MaxDepth: 2, MaxConcurrency: 1, PlanJSON: `{"schema_version":"loopcoder.child_plan.v1"}`, CreatedAt: at}
	edges := []RunEdgeRecord{{ParentRunID: parent.RunID, ChildRunID: children[0].RunID, RootRunID: parent.RootRunID, PlanID: plan.PlanID, ChildKey: "contract-child", Depth: 1, Ordinal: 0, ScopeJSON: `{"repo":".","paths":["src"],"issues":[1005]}`, Permission: "write", AggregationJSON: `{"mode":"collect","required":true}`, Status: "queued", CreatedAt: at, UpdatedAt: at}}
	requests := []ChildExecutionRequestRecord{{ChildRunID: children[0].RunID, ParentRunID: parent.RunID, PlanID: plan.PlanID, ChildKey: edges[0].ChildKey, SchemaVersion: "loopcoder.child_execution_request.v1", RequestJSON: `{"schema_version":"test.v1","permission":"write"}`, ContractFingerprint: "sha256:contract", RepositoryIdentity: "project:test", CheckoutIdentity: "/repo", Permission: "write", ScopeJSON: edges[0].ScopeJSON, LifecycleStatus: "queued", CreatedAt: at, UpdatedAt: at}}
	return parent, children, plan, edges, requests
}
