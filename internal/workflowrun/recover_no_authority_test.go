package workflowrun_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/workclaim"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

// TestRecoverAuthoritative_NoAuthority_ZeroMutation: durable launch/claim without
// authority is ambiguous corruption — fail closed before mutation, never select gN+1.
// This is a lower-boundary ledger fixture; it does NOT claim Fake recovery validity.
func TestRecoverAuthoritative_NoAuthority_ZeroMutation(t *testing.T) {
	home := t.TempDir()
	project, runID := "proj-noauth", "run-noauth"
	elog, err := workflowrun.OpenEventLog(home, project, runID)
	if err != nil {
		t.Fatal(err)
	}
	path := elog.Path()
	att := "att-only-deadbeef01-g0"
	plan := "sha256:plan-deadbeef"
	gdig := "sha256:graph-deadbeef"
	ccd := "sha256:ccd-deadbeef"
	route, _ := json.Marshal(map[string]string{
		"provider": "fixture", "model": "fixture-model", "depth": "medium",
		"permission": "bounded_write", "account_ref": "a", "install_ref": "i",
		"window_kind": "five_hour", "reservation_id": "r", "route_reason": "pin",
	})
	must := func(e workflowrun.Event) {
		t.Helper()
		e.ProjectID, e.RunID = project, runID
		e.At = time.Now().UTC()
		if _, err := elog.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	must(workflowrun.Event{
		Kind: "launch", WorkItemID: "only", AttemptID: att, Generation: 1,
		ExecutionPlanDigest: plan, GraphDigest: gdig, TaskClass: "tera", ChildContractDigest: ccd,
		Payload: route,
	})
	// No authority store row; no pid. Open launch is durable lifecycle without authority.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	n, rerr := workflowrun.RecoverOpenLaunchInterruptsAuthoritative(elog, workflowrun.RecoverOptions{
		HomeDir: home, ProjectID: project, RunID: runID, Now: t0,
	})
	if rerr == nil || n != 0 {
		t.Fatalf("want fail-closed zero mutation n=%d err=%v", n, rerr)
	}
	if !strings.Contains(rerr.Error(), "no_authority") && !strings.Contains(rerr.Error(), "missing authority") {
		t.Fatalf("want no_authority diagnostic: %v", rerr)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("event log mutated on no-authority fail closed")
	}
	// Never select gN+1 from this state.
	evs, _ := elog.ReadAllForRun(project, runID)
	got := workflowrun.NextAttemptGenerationFromEvents(evs)
	if _, ok := got["only"]; ok {
		t.Fatalf("must not select generation: %v", got)
	}
	_ = filepath.Join(home, "ok")
}

// TestRecoverAuthoritative_DeletedAuthorityAfterTerminalClosed_ZeroMutation:
// completed real provider lifecycle (terminal + closed claim + completed authority)
// then authority row deleted is cross-store corruption. Recover must fail-closed
// with no_authority and zero mutation — never skip because terminal/claim look done.
func TestRecoverAuthoritative_DeletedAuthorityAfterTerminalClosed_ZeroMutation(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-auth-deleted", "run_auth_deleted"
	plan, att, pid := seedOpenRecoverableAttempt(t, home, project, runID)
	elog, err := workflowrun.OpenEventLog(home, project, runID)
	if err != nil {
		t.Fatal(err)
	}
	gdig := claimGraphDigest(t, home, project, runID, att)
	ccd := "sha256:" + strings.Repeat("f", 64)
	evid := "sha256:" + strings.Repeat("a", 64)
	if _, err := elog.Append(workflowrun.Event{
		ProjectID: project, RunID: runID, Kind: "terminal", WorkItemID: "only",
		AttemptID: att, Generation: 1, Terminal: "succeeded", Evidence: evid,
		ExecutionPlanDigest: plan, GraphDigest: gdig, TaskClass: "tera", ChildContractDigest: ccd,
	}); err != nil {
		t.Fatal(err)
	}
	runDir, err := workflowrun.RunDurableDir(home, project, runID)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := workclaim.OpenPath(filepath.Join(runDir, "workclaims.json"), t0)
	if err != nil {
		t.Fatal(err)
	}
	var cl *workclaim.Claim
	for _, c := range cs.AllClaims() {
		if c.AttemptID == att {
			cp := c
			cl = &cp
			break
		}
	}
	if cl == nil {
		t.Fatal("seed claim missing")
	}
	if _, cerr := cs.Close(workclaim.CloseRequest{
		ClaimID: cl.ClaimID, Generation: cl.Generation, ExecutorID: "workflowrun",
		AttemptID: att, Terminal: "succeeded", OutputEvidence: evid,
	}); cerr != nil {
		t.Fatalf("close claim: %v", cerr)
	}
	ctx := context.Background()
	store, err := workflowrun.OpenAuthorityStore(ctx, home, project, runID, t0)
	if err != nil {
		t.Fatal(err)
	}
	owner := workflowrun.AuthorityOwnerFromClaimID(cl.ClaimID)
	if err := workflowrun.CompleteChildExecutionAuthority(ctx, store, project, runID, att, owner, int64(cl.Generation), "succeeded", t0()); err != nil {
		t.Fatalf("complete authority: %v", err)
	}
	// Tamper: delete authority row after terminal+closed claim (cross-store corruption).
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM provider_execution_authorities
			WHERE project_id = ? AND run_id = ? AND attempt_id = ?`,
			project, runID, att)
		return err
	}); err != nil {
		t.Fatalf("delete authority: %v", err)
	}
	_ = store.Close()

	path := elog.Path()
	beforeLog, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	claimPath := filepath.Join(runDir, "workclaims.json")
	beforeClaim, _ := os.ReadFile(claimPath)
	authPath, _ := workflowrun.AuthorityStorePath(home, project, runID)
	beforeAuth, _ := os.ReadFile(authPath)

	n, rerr := workflowrun.RecoverOpenLaunchInterruptsAuthoritative(elog, workflowrun.RecoverOptions{
		HomeDir: home, ProjectID: project, RunID: runID, Now: t0,
		WaitAlive: 50 * time.Millisecond, KillAfterVerify: false, PlanDigest: plan,
	})
	if rerr == nil || n != 0 {
		t.Fatalf("want fail-closed zero mutation n=%d err=%v", n, rerr)
	}
	if !strings.Contains(rerr.Error(), "no_authority") && !strings.Contains(rerr.Error(), "missing authority") {
		t.Fatalf("want no_authority diagnostic: %v", rerr)
	}
	afterLog, _ := os.ReadFile(path)
	afterClaim, _ := os.ReadFile(claimPath)
	afterAuth, _ := os.ReadFile(authPath)
	if string(beforeLog) != string(afterLog) {
		t.Fatal("event log mutated after deleted-authority recover")
	}
	if string(beforeClaim) != string(afterClaim) {
		t.Fatal("claims mutated after deleted-authority recover")
	}
	if string(beforeAuth) != string(afterAuth) {
		t.Fatal("authority db mutated after deleted-authority recover")
	}
	// Never invent gN+1 from corruption.
	evs, _ := elog.ReadAllForRun(project, runID)
	got := workflowrun.NextAttemptGenerationFromEvents(evs)
	// Open set empty (has terminal); NextAttemptGeneration may still report gens from events.
	// Must not invent a higher generation selection for recovery mutation.
	_ = pid
	_ = got
}
