package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	deliverypkg "github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestDeliveryCLIPlanDecideContinueJSON(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "loopcoder.db")
	seedCLIDeliveryRun(t, dbPath)

	var planOut, planErr bytes.Buffer
	code := RunWithDeps([]string{"delivery", "plan", "--db", dbPath, "--project-id", "proj_cli", "--run-id", "run_cli", "--format", "json"}, &planOut, &planErr, Deps{Now: fixedCLINow})
	if code != 0 {
		t.Fatalf("delivery plan exit = %d, stderr=%q", code, planErr.String())
	}
	var proposal deliverypkg.PlanProposal
	if err := json.Unmarshal(planOut.Bytes(), &proposal); err != nil {
		t.Fatalf("decode plan JSON: %v\n%s", err, planOut.String())
	}
	if proposal.AuthorizationFingerprint == "" || proposal.TaskCount != 1 {
		t.Fatalf("proposal = %#v, want fingerprinted one-task plan", proposal)
	}

	var decideOut, decideErr bytes.Buffer
	code = RunWithDeps([]string{"delivery", "decide", "--db", dbPath, "--project-id", "proj_cli", "--run-id", "run_cli", "--action", "approve", "--expected-authorization-fingerprint", proposal.AuthorizationFingerprint, "--idempotency-key", "cli-approve", "--format", "json"}, &decideOut, &decideErr, Deps{Now: fixedCLINow})
	if code != 0 {
		t.Fatalf("delivery decide exit = %d, stderr=%q", code, decideErr.String())
	}
	var decision deliverypkg.DecisionResult
	if err := json.Unmarshal(decideOut.Bytes(), &decision); err != nil {
		t.Fatalf("decode decision JSON: %v\n%s", err, decideOut.String())
	}
	if decision.Action != deliverypkg.DecisionActionApprove || decision.RunState != deliverypkg.RunApproved {
		t.Fatalf("decision = %#v, want approved", decision)
	}

	var continueOut, continueErr bytes.Buffer
	code = RunWithDeps([]string{"delivery", "continue", "--db", dbPath, "--project-id", "proj_cli", "--run-id", "run_cli", "--expected-authorization-fingerprint", proposal.AuthorizationFingerprint, "--idempotency-key", "cli-continue", "--format", "json"}, &continueOut, &continueErr, Deps{Now: fixedCLINow})
	if code != 0 {
		t.Fatalf("delivery continue exit = %d, stderr=%q", code, continueErr.String())
	}
	var continued deliverypkg.ContinueResult
	if err := json.Unmarshal(continueOut.Bytes(), &continued); err != nil {
		t.Fatalf("decode continue JSON: %v\n%s", err, continueOut.String())
	}
	if continued.RunState != deliverypkg.RunQueued {
		t.Fatalf("continue = %#v, want queued", continued)
	}
}

func TestDeliveryCLIExitCodesForPendingAndStale(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "loopcoder.db")
	seedCLIDeliveryRun(t, dbPath)

	var stdout, stderr bytes.Buffer
	code := RunWithDeps([]string{"delivery", "continue", "--db", dbPath, "--project-id", "proj_cli", "--run-id", "run_cli", "--format", "json"}, &stdout, &stderr, Deps{Now: fixedCLINow})
	if code != deliveryPendingApprovalExitCode {
		t.Fatalf("pending continue exit = %d, want %d; stderr=%q", code, deliveryPendingApprovalExitCode, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = RunWithDeps([]string{"delivery", "decide", "--db", dbPath, "--project-id", "proj_cli", "--run-id", "run_cli", "--action", "approve", "--expected-authorization-fingerprint", "sha256:" + strings.Repeat("0", 64), "--format", "json"}, &stdout, &stderr, Deps{Now: fixedCLINow})
	if code != deliveryStalePlanExitCode {
		t.Fatalf("stale decide exit = %d, want %d; stderr=%q", code, deliveryStalePlanExitCode, stderr.String())
	}
}

func seedCLIDeliveryRun(t *testing.T, dbPath string) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Options{Path: dbPath, Now: fixedCLINow})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT OR IGNORE INTO projects(id, local_path, created_at, updated_at) VALUES ('proj_cli', '/repo/proj_cli', ?, ?)`, deliverypkg.CanonicalTimestamp(fixedCLINow()), deliverypkg.CanonicalTimestamp(fixedCLINow()))
		return err
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	actor := deliverypkg.Actor{ActorKind: "user", ActorID: "local-user", DecisionAuthority: "user", Source: "test"}
	host := deliverypkg.Host{HostKind: "cli", HostID: "host-test", SessionID: "session-test", ProcessID: 1, LoopcoderVersion: "0.8.0", Platform: "test"}
	run := deliverypkg.DeliveryRun{
		DeliveryRunID:      "run_cli",
		ProjectID:          "proj_cli",
		State:              deliverypkg.RunAwaitingApproval,
		IntentSummary:      "cli delivery",
		MaxSideEffectClass: "provider-launch",
		ApprovalStatus:     "required",
		OverrideStatus:     "none",
		PolicyVersion:      "test-policy-v1",
		CreatedBy:          actor,
		UpdatedBy:          actor,
		Host:               host,
	}
	if _, err := deliverypkg.PersistDeliveryRun(ctx, store, run, deliverypkg.PersistOptions{IdempotencyKey: "cli-run", Now: fixedCLINow()}); err != nil {
		t.Fatalf("PersistDeliveryRun: %v", err)
	}
	task := deliverypkg.Task{
		ProjectID:        "proj_cli",
		DeliveryRunID:    "run_cli",
		TaskKey:          "a",
		Title:            "task a",
		State:            deliverypkg.TaskAwaitingApproval,
		RequirementsJSON: `{"do":"work"}`,
		ScopeJSON:        `{"paths":["README.md"]}`,
		Permission:       "write",
		SideEffectClass:  "repo-write",
		PolicyVersion:    "test-policy-v1",
		PlanFingerprint:  "sha256:" + strings.Repeat("3", 64),
		CreatedBy:        actor,
		UpdatedBy:        actor,
		Host:             host,
	}
	if _, err := deliverypkg.PersistTask(ctx, store, task, deliverypkg.PersistOptions{IdempotencyKey: "cli-task", Now: fixedCLINow()}); err != nil {
		t.Fatalf("PersistTask: %v", err)
	}
}
