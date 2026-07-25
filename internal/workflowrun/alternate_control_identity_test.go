package workflowrun_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun/testspawn"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

type exactControlIdentityExecutor struct {
	delegate workflowrun.ChildExecutor
	inputs   []workflowrun.ChildExecInput
}

func (e *exactControlIdentityExecutor) Execute(ctx context.Context, in workflowrun.ChildExecInput) (workflowrun.ChildExecResult, error) {
	e.inputs = append(e.inputs, in)
	if strings.TrimSpace(in.ProjectID) == "" || strings.TrimSpace(in.RunID) == "" ||
		strings.TrimSpace(in.AttemptID) == "" {
		err := fmt.Errorf("test executor: project_id, run_id, and attempt_id required")
		return workflowrun.ChildExecResult{
			Terminal: workgraph.TermFailed, FailureClass: "control_plane_path",
			Message: err.Error(),
		}, err
	}
	return e.delegate.Execute(ctx, in)
}

// TestModelUnavailableAlternate_PreservesProviderControlIdentity exercises the
// real Service reroute lifecycle with spawned-process evidence. It guards the
// production executor boundary: both the failed primary and successful
// alternate must receive the exact project/run/attempt namespace needed for
// the external provider control-plane log and guardian paths.
func TestModelUnavailableAlternate_PreservesProviderControlIdentity(t *testing.T) {
	home := testHome(t)
	repo := initGitRepo(t)
	const project = "proj-alt-control"
	const runID = "run_alt_control"

	recorder := &exactControlIdentityExecutor{delegate: testspawn.Executor{
		HomeDir: home, Now: t0, FailModel: "model-unavailable-token",
		ProductFiles: map[string][]string{"only": {"slug/slug.go"}},
	}}
	svc := workflowrun.Service{Now: t0, HomeDir: home, Executor: recorder}
	res, err := svc.Execute(context.Background(), withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: project, RunID: runID, RepoPath: repo,
		Integrator: &countingEnsureIntegrator{},
		Definition: workflowrun.OneNodeDefinition("g-alt-control", "implement slug package"),
		Actor:      "owner", CapacityReroute: passThroughCapHook{},
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {
				Provider: "codex", Model: "model-unavailable-token", TaskClass: "tera",
				Depth: "medium", Permission: "bounded_write",
				AccountRef: "acct-codex", InstallRef: "install-codex", WindowKind: "weekly",
				ReservationID: "res-primary", RouteReason: "canary-probe",
			},
		},
		SameDepthAlternates: map[string][]workflowrun.AlternateCandidate{
			"only": {
				{Provider: "codex", Model: "model-unavailable-token", Effort: "medium",
					Permission: "bounded_write", AccountRef: "acct-codex", InstallRef: "install-codex",
					WindowKind: "weekly", HardEligible: true},
				{Provider: "codex", Model: "gpt-5.3-codex-spark", Effort: "medium",
					Permission: "bounded_write", AccountRef: "acct-codex", InstallRef: "install-codex",
					WindowKind: "weekly", HardEligible: true},
			},
		},
	}))
	if err != nil {
		t.Fatalf("execute: %v status=%s message=%s children=%+v", err, res.Status, res.Message, res.Children)
	}
	if res.Status != workflowrun.StatusHumanGate {
		t.Fatalf("status=%s message=%s", res.Status, res.Message)
	}
	if len(recorder.inputs) != 2 {
		t.Fatalf("executor calls=%d want primary+alternate", len(recorder.inputs))
	}
	for i, in := range recorder.inputs {
		if in.ProjectID != project || in.RunID != runID || strings.TrimSpace(in.AttemptID) == "" {
			t.Fatalf("input[%d] control identity project=%q run=%q attempt=%q", i, in.ProjectID, in.RunID, in.AttemptID)
		}
	}
	if !strings.HasSuffix(recorder.inputs[0].AttemptID, "-g0") ||
		!strings.HasSuffix(recorder.inputs[1].AttemptID, "-g1") ||
		recorder.inputs[0].AttemptID == recorder.inputs[1].AttemptID {
		t.Fatalf("attempt lineage primary=%q alternate=%q", recorder.inputs[0].AttemptID, recorder.inputs[1].AttemptID)
	}

	events := loadEvents(t, home, project, runID)
	altPID, altTerminal := 0, 0
	for _, ev := range events {
		if ev.WorkItemID != "only" || ev.AttemptID != recorder.inputs[1].AttemptID {
			continue
		}
		if ev.Kind == "pid" && ev.PID > 0 {
			altPID++
		}
		if ev.Kind == "terminal" && ev.Terminal == string(workgraph.TermSucceeded) {
			altTerminal++
		}
	}
	if altPID != 1 || altTerminal != 1 {
		t.Fatalf("alternate evidence pid=%d succeeded_terminal=%d", altPID, altTerminal)
	}

	successes := 0
	for _, child := range res.Children {
		if child.WorkItemID == "only" && child.Terminal == string(workgraph.TermSucceeded) {
			successes++
			if child.SupersedesAttemptID != recorder.inputs[0].AttemptID {
				t.Fatalf("alternate supersedes=%q want %q", child.SupersedesAttemptID, recorder.inputs[0].AttemptID)
			}
		}
	}
	if successes != 1 || res.LaunchCount != 2 || res.ClaimCount != 2 {
		t.Fatalf("successes=%d launches=%d claims=%d", successes, res.LaunchCount, res.ClaimCount)
	}
}
