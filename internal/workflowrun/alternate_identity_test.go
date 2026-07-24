package workflowrun_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/workclaim"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

// TestModelUnavailableAlternate_ContractIdentityCrossStore proves original and
// alternate share CCD/plan/class while AttemptID and positive generation change,
// with claim store and event log agreement and no duplicate success.
func TestModelUnavailableAlternate_ContractIdentityCrossStore(t *testing.T) {
	home := testHome(t)
	now := t0()
	calls := map[string]int{}
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: t0, Calls: calls,
			FailModel: "model-unavailable-token",
		},
	}
	res, err := svc.Execute(context.Background(), withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: "proj-alt-ccd", RunID: "run_alt_ccd",
		Definition: workflowrun.OneNodeDefinition("g-alt-ccd", "implement alternate identity"),
		Actor:      "owner", CapacityReroute: passThroughCapHook{},
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {
				Provider: "antigravity", Model: "model-unavailable-token", TaskClass: "tera",
				Depth: "medium", Permission: "bounded_write",
				AccountRef: "acct-ag", InstallRef: "install-ag", WindowKind: "five_hour",
				ReservationID: "res-ag-prior", RouteReason: "pin-bad",
			},
		},
		SameDepthAlternates: map[string][]workflowrun.AlternateCandidate{
			"only": {
				{Provider: "antigravity", Model: "model-unavailable-token", Effort: "medium",
					Permission: "bounded_write", AccountRef: "acct-ag", InstallRef: "install-ag",
					WindowKind: "five_hour", HardEligible: true},
				{Provider: "codex", Model: "gpt-5.5", Effort: "medium",
					Permission: "bounded_write", AccountRef: "acct-codex", InstallRef: "install-codex",
					WindowKind: "five_hour", HardEligible: true},
			},
		},
	}))
	if err != nil {
		t.Fatalf("execute: %v status=%s msg=%s", err, res.Status, res.Message)
	}
	if res.Status != workflowrun.StatusHumanGate {
		t.Fatalf("status=%s msg=%s children=%+v", res.Status, res.Message, res.Children)
	}

	var failed, ok *workflowrun.ChildOutcome
	for i := range res.Children {
		c := &res.Children[i]
		if c.WorkItemID != "only" {
			continue
		}
		if c.FailureClass == "model_unavailable" {
			failed = c
		}
		if c.Terminal == "succeeded" {
			ok = c
		}
	}
	if failed == nil || ok == nil {
		t.Fatalf("want failed+success: %+v", res.Children)
	}

	// Same assignment contract across alternates.
	if failed.ChildContractDigest == "" || len(strings.TrimPrefix(failed.ChildContractDigest, "sha256:")) != 64 {
		t.Fatalf("failed CCD not full sha256: %q", failed.ChildContractDigest)
	}
	if failed.ChildContractDigest != ok.ChildContractDigest {
		t.Fatalf("CCD mismatch failed=%q ok=%q", failed.ChildContractDigest, ok.ChildContractDigest)
	}
	if failed.ExecutionPlanDigest != res.PlanDigest || ok.ExecutionPlanDigest != res.PlanDigest {
		t.Fatalf("plan digests failed=%q ok=%q res=%q", failed.ExecutionPlanDigest, ok.ExecutionPlanDigest, res.PlanDigest)
	}
	if failed.TaskClass != "tera" || ok.TaskClass != "tera" {
		t.Fatalf("task class failed=%q ok=%q", failed.TaskClass, ok.TaskClass)
	}
	if failed.AttemptID == ok.AttemptID {
		t.Fatalf("attempt ids must differ")
	}
	if failed.Generation < 1 || ok.Generation < 1 {
		t.Fatalf("positive generation failed=%d ok=%d", failed.Generation, ok.Generation)
	}
	if ok.Generation <= failed.Generation {
		t.Fatalf("generation must increase failed=%d ok=%d", failed.Generation, ok.Generation)
	}
	if ok.SupersedesAttemptID != failed.AttemptID {
		t.Fatalf("supersedes=%q want %q", ok.SupersedesAttemptID, failed.AttemptID)
	}
	// Alternate must retain InstallRef / ActualSources / ArgvDigest when known.
	if ok.InstallRef == "" {
		t.Fatalf("alternate missing InstallRef: %+v", ok)
	}

	successN := 0
	for _, c := range res.Children {
		if c.WorkItemID == "only" && c.Terminal == "succeeded" {
			successN++
		}
	}
	if successN != 1 {
		t.Fatalf("want exactly one success, got %d", successN)
	}
	if calls["only"] != 2 {
		t.Fatalf("calls=%v want 2", calls)
	}

	// Claim store.
	claimPath := filepath.Join(home, "projects", "proj-alt-ccd", "runs", "run_alt_ccd", "workclaims.json")
	cs, err := workclaim.OpenPath(claimPath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	cf, err := cs.GetByAttempt("proj-alt-ccd", res.GraphID, res.GraphVersion, "only", failed.AttemptID)
	if err != nil {
		t.Fatalf("failed claim: %v", err)
	}
	ca, err := cs.GetByAttempt("proj-alt-ccd", res.GraphID, res.GraphVersion, "only", ok.AttemptID)
	if err != nil {
		t.Fatalf("alt claim: %v", err)
	}
	if cf.ChildContractDigest != failed.ChildContractDigest || ca.ChildContractDigest != failed.ChildContractDigest {
		t.Fatalf("claim CCD mismatch")
	}
	if cf.PlanDigest != res.PlanDigest || ca.PlanDigest != res.PlanDigest {
		t.Fatalf("claim plan mismatch")
	}
	if ca.Generation <= cf.Generation {
		t.Fatalf("claim gen failed=%d alt=%d", cf.Generation, ca.Generation)
	}

	// Events.
	raw, err := os.ReadFile(res.EventLogPath)
	if err != nil {
		t.Fatal(err)
	}
	events, err := workflowrun.ParseEventJSONLStrict(string(raw), "proj-alt-ccd", "run_alt_ccd")
	if err != nil {
		t.Fatal(err)
	}
	var sawMU, sawAltLaunch bool
	for _, ev := range events {
		if ev.WorkItemID != "only" {
			continue
		}
		if ev.ChildContractDigest != "" && ev.ChildContractDigest != failed.ChildContractDigest {
			t.Fatalf("event %s ccd %q", ev.Kind, ev.ChildContractDigest)
		}
		if ev.Kind == "model_unavailable" {
			sawMU = true
			if ev.Generation != failed.Generation {
				t.Fatalf("mu gen %d want %d", ev.Generation, failed.Generation)
			}
		}
		if ev.Kind == "launch" && ev.AttemptID == ok.AttemptID {
			sawAltLaunch = true
			if ev.Generation != ok.Generation {
				t.Fatalf("alt launch gen %d want %d", ev.Generation, ok.Generation)
			}
			if ev.TaskClass != "tera" {
				t.Fatalf("alt launch task_class %q", ev.TaskClass)
			}
		}
	}
	if !sawMU || !sawAltLaunch {
		t.Fatalf("events incomplete mu=%v altLaunch=%v", sawMU, sawAltLaunch)
	}

	// Partial must include succeeded alternate under PriorSucceeded.
	partial, err := workflowrun.LoadPartialPrior(home, "proj-alt-ccd", "run_alt_ccd")
	if err != nil {
		t.Fatal(err)
	}
	if partial.PlanDigest != res.PlanDigest || partial.GraphDigest != res.GraphDigest {
		t.Fatalf("partial digests plan=%q graph=%q", partial.PlanDigest, partial.GraphDigest)
	}
	ps, okP := partial.PriorSucceeded["only"]
	if !okP {
		t.Fatal("partial.PriorSucceeded missing only")
	}
	if ps.ChildContractDigest != failed.ChildContractDigest || ps.AttemptID != ok.AttemptID {
		t.Fatalf("partial prior: %+v", ps)
	}
	if ps.Generation != ok.Generation {
		t.Fatalf("partial gen %d want %d", ps.Generation, ok.Generation)
	}
}
