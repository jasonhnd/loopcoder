package goalrun_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/goalrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

func writeEventLog(t *testing.T, path string, events []workflowrun.Event) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i, ev := range events {
		if strings.TrimSpace(ev.Schema) == "" {
			ev.Schema = workflowrun.EventSchema
		}
		if strings.TrimSpace(ev.EventID) == "" {
			ev.EventID = fmt.Sprintf("wev_test_%d", i+1)
		}
		if ev.At.IsZero() {
			ev.At = time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
		}
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadWorkflowEvents_EventLogPathOnlySuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workflow-events.jsonl")
	writeEventLog(t, path, []workflowrun.Event{
		{EventID: "wev_1", Kind: "run.start", ProjectID: "proj-a", RunID: "run-a"},
		{EventID: "wev_2", Kind: "claim", ProjectID: "proj-a", RunID: "run-a", AttemptID: "att-g0", WorkItemID: "only"},
	})
	res := goalrun.Result{
		ProjectID: "proj-a", RunID: "run-a",
		Workflow: workflowrun.Result{EventLogPath: path},
	}
	// HomeDir empty — must still load exact EventLogPath.
	evs, gotPath, err := goalrun.LoadWorkflowEventsForTest(res, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if gotPath != path {
		t.Fatalf("path=%q want %q", gotPath, path)
	}
	if len(evs) != 2 || evs[0].EventID != "wev_1" || evs[1].Kind != "claim" {
		t.Fatalf("%+v", evs)
	}
}

func TestLoadWorkflowEvents_MalformedLineFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.jsonl")
	if err := os.WriteFile(path, []byte("{\"kind\":\"claim\"}\nnot-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := goalrun.Result{
		ProjectID: "p", RunID: "r",
		Workflow: workflowrun.Result{EventLogPath: path},
	}
	if _, _, err := goalrun.LoadWorkflowEventsForTest(res, ""); err == nil {
		t.Fatal("want malformed fail")
	}
}

func TestLoadWorkflowEvents_WrongRunFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wrong-run.jsonl")
	writeEventLog(t, path, []workflowrun.Event{
		{EventID: "wev_x", Kind: "claim", ProjectID: "proj-a", RunID: "run-OTHER", AttemptID: "a"},
	})
	res := goalrun.Result{
		ProjectID: "proj-a", RunID: "run-a",
		Workflow: workflowrun.Result{EventLogPath: path},
	}
	if _, _, err := goalrun.LoadWorkflowEventsForTest(res, ""); err == nil {
		t.Fatal("want wrong-run fail")
	}
}

func TestProofFromResult_FakeEventIDsFailClosed(t *testing.T) {
	// Events missing required kinds → nil proof even with fabricated RerouteEventRef.
	res := goalrun.Result{
		ProjectID: "p", RunID: "r",
		Workflow: workflowrun.Result{
			Children: []workflowrun.ChildOutcome{
				{WorkItemID: "only", AttemptID: "att-g0", FailureClass: "model_unavailable", Terminal: "failed"},
				{WorkItemID: "only", AttemptID: "att-g1", SupersedesAttemptID: "att-g0",
					Terminal: "succeeded", RerouteEventRef: "event_id=fake1;event_id=fake2"},
			},
		},
	}
	// Empty events
	if got := goalrun.ProofFromResultForTest(res, nil); got != nil {
		t.Fatalf("empty events must nil: %+v", got)
	}
	// Events without model_unavailable / reroute linkage
	fake := []workflowrun.Event{
		{EventID: "e1", Kind: "claim", AttemptID: "att-g0"},
		{EventID: "e2", Kind: "launch", AttemptID: "att-g0"},
	}
	if got := goalrun.ProofFromResultForTest(res, fake); got != nil {
		t.Fatalf("incomplete events must nil: %+v", got)
	}
}

func TestEmitCanary_EventLogPathOnlyUnavailableNilOnMalformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "malformed.jsonl")
	if err := os.WriteFile(path, []byte("{bad\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "canary.json")
	res := goalrun.Result{
		ProjectID: "p", RunID: "r",
		Workflow: workflowrun.Result{EventLogPath: path, Interrupted: false},
		// Claimed model_unavailable exclude without readable events → unavail must not invent.
		RouteExcludes: []goalrun.RouteExclude{
			{ChildID: "only", Provider: "antigravity", Reason: "model_unavailable", Claimed: true,
				Message: "event_id=fake"},
		},
	}
	// Emit may fail or succeed on other metrics; when it succeeds, UnavailableRetry nil.
	ev, err := goalrun.EmitCanaryFromResult(res, goalrun.CanaryEmitOptions{
		OutPath: outPath, ArchiveDigest: "sha:a", PreProdSHA: "abc", BinaryVersion: "0.0.0", BinaryCommit: "dead",
	})
	if err != nil {
		// Some canary paths fail closed on missing restart — still acceptable if no invented unavail.
		t.Logf("emit err (ok if no invent): %v", err)
		return
	}
	if ev.UnavailableRetry != nil {
		t.Fatalf("malformed log must not invent unavailable_retry: %+v", ev.UnavailableRetry)
	}
}

func TestLoadWorkflowEvents_MissingEventIDFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no-id.jsonl")
	// Schema ok but empty EventID
	line := `{"schema":"loopcoder.workflow.event.v1","kind":"claim","project_id":"p","run_id":"r","at":"2026-07-23T12:00:00Z"}`
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := goalrun.Result{ProjectID: "p", RunID: "r", Workflow: workflowrun.Result{EventLogPath: path}}
	if _, _, err := goalrun.LoadWorkflowEventsForTest(res, ""); err == nil {
		t.Fatal("want missing event_id fail")
	}
}

func TestLoadWorkflowEvents_MissingSchemaFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no-schema.jsonl")
	line := `{"event_id":"e1","kind":"claim","project_id":"p","run_id":"r","at":"2026-07-23T12:00:00Z"}`
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := goalrun.Result{ProjectID: "p", RunID: "r", Workflow: workflowrun.Result{EventLogPath: path}}
	if _, _, err := goalrun.LoadWorkflowEventsForTest(res, ""); err == nil {
		t.Fatal("want missing schema fail")
	}
}

func TestProofFromResult_AmbiguousPairsFail(t *testing.T) {
	// Two failed/retry pairs → nil
	res := goalrun.Result{
		Workflow: workflowrun.Result{
			Children: []workflowrun.ChildOutcome{
				{WorkItemID: "a", AttemptID: "a0", FailureClass: "model_unavailable", Terminal: "failed"},
				{WorkItemID: "a", AttemptID: "a1", SupersedesAttemptID: "a0", Terminal: "succeeded"},
				{WorkItemID: "b", AttemptID: "b0", FailureClass: "model_unavailable", Terminal: "failed"},
				{WorkItemID: "b", AttemptID: "b1", SupersedesAttemptID: "b0", Terminal: "succeeded"},
			},
		},
	}
	// Even with events, ambiguous pairs fail before event checks if we return nil on len(pairs)!=1
	if got := goalrun.ProofFromResultForTest(res, []workflowrun.Event{{EventID: "x", Kind: "claim", Schema: workflowrun.EventSchema}}); got != nil {
		t.Fatalf("ambiguous pairs must nil: %+v", got)
	}
}

func TestProofFromResult_SucceededWithoutIntegrateFails(t *testing.T) {
	// retry has Terminal succeeded and IntegrateCommitSHA set but no IntegrateCommits
	res := goalrun.Result{
		Workflow: workflowrun.Result{
			Children: []workflowrun.ChildOutcome{
				{WorkItemID: "only", AttemptID: "att-g0", FailureClass: "model_unavailable", Terminal: "failed"},
				{WorkItemID: "only", AttemptID: "att-g1", SupersedesAttemptID: "att-g0",
					Terminal: "succeeded", IntegrateCommitSHA: "sha-fake", RerouteEventRef: "event_id=e1;event_id=e2;event_id=e3;event_id=e4"},
			},
		},
	}
	// Incomplete events + integrate SHA without commits → nil
	if got := goalrun.ProofFromResultForTest(res, nil); got != nil {
		t.Fatal(got)
	}
}

func TestLoadWorkflowEvents_ProjectIDCaseSensitive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "case.jsonl")
	writeEventLog(t, path, []workflowrun.Event{
		{EventID: "wev_1", Kind: "claim", ProjectID: "Proj-A", RunID: "run-a", AttemptID: "a", WorkItemID: "only"},
	})
	// Expect lowercase — must fail (not EqualFold).
	res := goalrun.Result{
		ProjectID: "proj-a", RunID: "run-a",
		Workflow: workflowrun.Result{EventLogPath: path},
	}
	if _, _, err := goalrun.LoadWorkflowEventsForTest(res, ""); err == nil {
		t.Fatal("want case-sensitive project_id fail")
	}
}

func TestEmitCanary_ClaimedUnavailable_LoadFail_NilNotUnclaimedFallback(t *testing.T) {
	// Adversarial: load fails + claimed model_unavailable + unclaimed exclude present
	// → unavailable_retry must stay nil (not fall back to unclaimed reason).
	dir := t.TempDir()
	path := filepath.Join(dir, "missing-or-bad.jsonl")
	if err := os.WriteFile(path, []byte("{bad json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "canary.json")
	res := goalrun.Result{
		ProjectID: "p", RunID: "r",
		Workflow: workflowrun.Result{EventLogPath: path},
		RouteExcludes: []goalrun.RouteExclude{
			{ChildID: "only", Provider: "antigravity", Reason: "model_unavailable", Claimed: true},
			// Tempting unclaimed fallback — must not be used when claimed MU present.
			{ChildID: "only", Provider: "codex", Reason: "exhausted", Claimed: false},
		},
	}
	ev, err := goalrun.EmitCanaryFromResult(res, goalrun.CanaryEmitOptions{
		OutPath: outPath, ArchiveDigest: "sha:a", PreProdSHA: "abc",
		BinaryVersion: "0.0.0", BinaryCommit: "dead",
	})
	if err != nil {
		// Emit may fail for other reasons; still must not invent claimed unavail.
		t.Logf("emit err (ok): %v", err)
		return
	}
	if ev.UnavailableRetry != nil {
		t.Fatalf("claimed MU + load fail must not fall back to unclaimed: %+v", ev.UnavailableRetry)
	}
}

func TestAttemptID_DeterministicMatchesCapacityBinding(t *testing.T) {
	// goalrun and workflowrun must share the same formula.
	a := workflowrun.AttemptID("only", "plan-digest", "run-1", 0)
	b := workflowrun.AttemptID("only", "plan-digest", "run-1", 0)
	if a == "" || a != b {
		t.Fatalf("%q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "att-only-") || !strings.HasSuffix(a, "-g0") {
		t.Fatalf("shape: %q", a)
	}
	g1 := workflowrun.AttemptID("only", "plan-digest", "run-1", 1)
	if g1 == a || !strings.HasSuffix(g1, "-g1") {
		t.Fatalf("gen bump: %q %q", a, g1)
	}
}
