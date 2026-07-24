package workflowrun_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
