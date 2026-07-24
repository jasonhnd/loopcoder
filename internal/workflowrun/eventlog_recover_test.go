package workflowrun_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

func TestRecoverOpenLaunchInterrupts_FromLedgerOnly(t *testing.T) {
	home := t.TempDir()
	elog, err := workflowrun.OpenEventLog(home, "proj-a", "run-a")
	if err != nil {
		t.Fatal(err)
	}
	// research succeeded; implement launched mid-kill with no terminal.
	must := func(e workflowrun.Event) {
		t.Helper()
		e.ProjectID, e.RunID = "proj-a", "run-a"
		if e.At.IsZero() {
			e.At = time.Now().UTC()
		}
		if _, err := elog.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	must(workflowrun.Event{Kind: "launch", WorkItemID: "wi_research", AttemptID: "att-r-g0", Generation: 1})
	must(workflowrun.Event{Kind: "terminal", WorkItemID: "wi_research", AttemptID: "att-r-g0", Generation: 1, Terminal: "succeeded", Evidence: "sha256:abc"})
	must(workflowrun.Event{Kind: "integrate", WorkItemID: "wi_research", AttemptID: "att-r-g0", Generation: 1, CommitSHA: "deadbeef"})
	must(workflowrun.Event{Kind: "launch", WorkItemID: "wi_implement", AttemptID: "att-i-g0", Generation: 1})

	n, err := workflowrun.RecoverOpenLaunchInterrupts(elog, "proj-a", "run-a")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want 1 recovered interrupt, got %d", n)
	}
	// idempotent
	n2, err := workflowrun.RecoverOpenLaunchInterrupts(elog, "proj-a", "run-a")
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Fatalf("second recover must be no-op, got %d", n2)
	}
	events, err := elog.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	interrupted, aborted := workflowrun.InterruptedFromEvents(events)
	if !interrupted {
		t.Fatal("want interrupted from recovered ledger")
	}
	if aborted["wi_implement"] != "att-i-g0" {
		t.Fatalf("aborted=%v", aborted)
	}
	if _, ok := aborted["wi_research"]; ok {
		t.Fatalf("succeeded item must not be aborted: %v", aborted)
	}
	// no open launches → 0
	home2 := t.TempDir()
	elog2, _ := workflowrun.OpenEventLog(home2, "p", "r")
	_, _ = elog2.Append(workflowrun.Event{Kind: "launch", WorkItemID: "a", AttemptID: "att-a-x-g0", Generation: 1, ProjectID: "p", RunID: "r"})
	_, _ = elog2.Append(workflowrun.Event{Kind: "terminal", WorkItemID: "a", AttemptID: "att-a-x-g0", Generation: 1, Terminal: "succeeded", ProjectID: "p", RunID: "r"})
	n3, err := workflowrun.RecoverOpenLaunchInterrupts(elog2, "p", "r")
	if err != nil || n3 != 0 {
		t.Fatalf("complete run recover n=%d err=%v", n3, err)
	}
	_ = os.WriteFile(filepath.Join(home, "ok"), []byte("1"), 0o600)
}

func TestOpenLaunchesWithoutTerminal(t *testing.T) {
	// Attempt IDs need -gN suffix for generation-aware latest-open reduction.
	evs := []workflowrun.Event{
		{Kind: "launch", WorkItemID: "a", AttemptID: "att-a-x-g0", Generation: 1},
		{Kind: "launch", WorkItemID: "b", AttemptID: "att-b-x-g0", Generation: 1},
		{Kind: "terminal", WorkItemID: "a", AttemptID: "att-a-x-g0", Generation: 1},
	}
	open := workflowrun.OpenLaunchesWithoutTerminal(evs)
	if len(open) != 1 || open["b"] != "att-b-x-g0" {
		t.Fatalf("open=%v", open)
	}
}

func TestFailedRetryGenerationsBumpsPastTerminalFailed(t *testing.T) {
	evs := []workflowrun.Event{
		{Kind: "launch", WorkItemID: "wi_research", AttemptID: "att-wi_research-x-g0"},
		{Kind: "terminal", WorkItemID: "wi_research", AttemptID: "att-wi_research-x-g0", Terminal: "succeeded"},
		{Kind: "integrate", WorkItemID: "wi_research", AttemptID: "att-wi_research-x-g0"},
		{Kind: "launch", WorkItemID: "wi_implement", AttemptID: "att-wi_implement-x-g0"},
		{Kind: "interrupt", WorkItemID: "wi_implement", AttemptID: "att-wi_implement-x-g0", Terminal: "cancelled"},
		{Kind: "launch", WorkItemID: "wi_implement", AttemptID: "att-wi_implement-x-g1"},
		{Kind: "terminal", WorkItemID: "wi_implement", AttemptID: "att-wi_implement-x-g1", Terminal: "failed"},
	}
	got := workflowrun.FailedRetryGenerations(evs)
	if got["wi_implement"] != 2 {
		t.Fatalf("implement next gen=%v want 2", got)
	}
	if _, ok := got["wi_research"]; ok {
		t.Fatalf("research should not retry-bump: %v", got)
	}
}
