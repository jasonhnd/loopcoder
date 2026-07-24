package workflowrun_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// preSpawnFailExecutor returns a terminal failure with empty InvokedRoute.Model
// (RC38 production shape: preflight/auth fails before OnProviderStart and never
// affirms ActualModel). Used to prove Service does not overwrite the original
// FailureClass with route_identity_mismatch.
type preSpawnFailExecutor struct {
	FailureClass string
	Message      string
}

func (e preSpawnFailExecutor) Execute(ctx context.Context, in workflowrun.ChildExecInput) (workflowrun.ChildExecResult, error) {
	_ = ctx
	fc := e.FailureClass
	if fc == "" {
		fc = "auth_refusal"
	}
	msg := e.Message
	if msg == "" {
		msg = "codex: account mismatch requested=acct-a active=acct-b"
	}
	return workflowrun.ChildExecResult{
		Terminal:     workgraph.TermFailed,
		FailureClass: fc,
		Message:      msg,
		// Provider only — model/depth/account unobserved (pre-spawn).
		InvokedRoute: workflowrun.ChildRoute{Provider: "codex"},
		ActualSource: "unknown",
	}, fmt.Errorf("%s", msg)
}

// successEmptyModelExecutor claims success without affirming InvokedRoute.Model.
type successEmptyModelExecutor struct{}

func (successEmptyModelExecutor) Execute(ctx context.Context, in workflowrun.ChildExecInput) (workflowrun.ChildExecResult, error) {
	_ = ctx
	r := in.Route
	r.Model = "" // drop model affirmation while still claiming success
	return workflowrun.ChildExecResult{
		Terminal:       workgraph.TermSucceeded,
		OutputEvidence: "sha256:" + strings.Repeat("a", 64),
		InvokedRoute:   r,
		Provider:       r.Provider,
		Model:          "",
		Depth:          r.Depth,
		ActualSource:   "unknown",
	}, nil
}

func TestPreSpawnFailure_PreservesOriginalFailureClass(t *testing.T) {
	home := testHome(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: preSpawnFailExecutor{
			FailureClass: "auth_refusal",
			Message:      "codex: account mismatch requested=acct-status active=acct-canonical",
		},
	}
	route := workflowrun.ChildRoute{
		Provider: "codex", Model: "codex-auto-review", TaskClass: "tera", Depth: "medium",
		Permission: "bounded_write", AccountRef: "acct-" + strings.Repeat("a", 64),
		InstallRef: "pinst_test", WindowKind: "weekly", ReservationID: "sres_1",
	}
	res, err := svc.Execute(context.Background(), withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: "proj-preservefc", RunID: "run_preserve_fc",
		Definition:  workflowrun.OneNodeDefinition("g-preserve", "research"),
		Actor:       "owner",
		ChildRoutes: map[string]workflowrun.ChildRoute{"only": route},
	}))
	_ = err
	if len(res.Children) == 0 {
		t.Fatalf("no children: status=%s msg=%s", res.Status, res.Message)
	}
	c := res.Children[0]
	if c.Terminal == "succeeded" {
		t.Fatalf("must not succeed: %+v", c)
	}
	if c.FailureClass == "route_identity_mismatch" {
		t.Fatalf("must preserve original auth_refusal, not overwrite with route_identity_mismatch: %+v", c)
	}
	if c.FailureClass != "auth_refusal" {
		t.Fatalf("FailureClass=%q want auth_refusal (message=%q)", c.FailureClass, c.Message)
	}
	if !strings.Contains(c.Message, "account mismatch") {
		t.Fatalf("Message must retain preflight text, got %q", c.Message)
	}
}

func TestSuccessEmptyInvokedModel_RouteIdentityMismatch(t *testing.T) {
	home := testHome(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: successEmptyModelExecutor{},
	}
	route := workflowrun.ChildRoute{
		Provider: "fixture", Model: "fixture-model", TaskClass: "tera", Depth: "medium",
		Permission: "bounded_write", AccountRef: "acct-f", WindowKind: "five_hour", ReservationID: "res-f",
	}
	res, err := svc.Execute(context.Background(), withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: "proj-empty-model", RunID: "run_empty_model",
		Definition:  workflowrun.OneNodeDefinition("g-empty-model", "impl"),
		Actor:       "owner",
		ChildRoutes: map[string]workflowrun.ChildRoute{"only": route},
	}))
	_ = err
	var found bool
	for _, c := range res.Children {
		if c.Terminal == "succeeded" {
			t.Fatalf("success without affirmed model must fail closed: %+v", c)
		}
		if c.FailureClass == "route_identity_mismatch" {
			found = true
			if !strings.Contains(c.Message, "invoked model") {
				t.Fatalf("want invoked model message, got %q", c.Message)
			}
		}
	}
	if !found {
		t.Fatalf("want route_identity_mismatch on success-without-model, children=%+v status=%s", res.Children, res.Status)
	}
}

// materializeFailExecutor mimics Production research findings materialization failure
// after provider success: typed class + observed InvokedRoute, terminal failed.
type materializeFailExecutor struct{}

func (materializeFailExecutor) Execute(ctx context.Context, in workflowrun.ChildExecInput) (workflowrun.ChildExecResult, error) {
	_ = ctx
	msg := "research findings dest is not a regular file or symlink (mode=drwx------)"
	return workflowrun.ChildExecResult{
		Terminal:     workgraph.TermFailed,
		FailureClass: workflowrun.FailureClassResearchFindingsMaterialization,
		Message:      msg,
		Provider:     in.Route.Provider,
		Model:        in.Route.Model,
		Depth:        in.Route.Depth,
		InvokedRoute: workflowrun.ChildRoute{
			Provider: in.Route.Provider, Model: in.Route.Model, Depth: in.Route.Depth,
			Permission: in.Route.Permission, AccountRef: in.Route.AccountRef,
			InstallRef: in.Route.InstallRef, WindowKind: in.Route.WindowKind,
			ReservationID: in.Route.ReservationID,
		},
		ActualSource: "unknown",
	}, fmt.Errorf("%s", msg)
}

func TestService_ResearchFindingsMaterialization_PreservesTypedFailure(t *testing.T) {
	home := testHome(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: materializeFailExecutor{},
	}
	route := workflowrun.ChildRoute{
		Provider: "codex", Model: "codex-auto-review", TaskClass: "tera", Depth: "medium",
		Permission: "bounded_write", AccountRef: "acct-" + strings.Repeat("c", 64),
		InstallRef: "pinst_test", WindowKind: "weekly", ReservationID: "sres_1",
	}
	res, _ := svc.Execute(context.Background(), withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: "proj-matfail", RunID: "run_matfail",
		Definition:  workflowrun.OneNodeDefinition("g-matfail", "research"),
		Actor:       "owner",
		ChildRoutes: map[string]workflowrun.ChildRoute{"only": route},
	}))
	if len(res.Children) == 0 {
		t.Fatalf("no children: %+v", res)
	}
	c := res.Children[0]
	if c.FailureClass == "route_identity_mismatch" || c.FailureClass == "missing_evidence" {
		t.Fatalf("must not overwrite materialize failure: %+v", c)
	}
	if c.FailureClass != workflowrun.FailureClassResearchFindingsMaterialization {
		t.Fatalf("FailureClass=%q want %q msg=%q", c.FailureClass, workflowrun.FailureClassResearchFindingsMaterialization, c.Message)
	}
	if !strings.Contains(c.Message, "regular") && !strings.Contains(c.Message, "directory") {
		t.Fatalf("message must retain dest reason: %q", c.Message)
	}
}

func TestGrokPreSpawnFailure_PreservesOriginalFailureClass(t *testing.T) {
	// Shared service path must preserve non-codex provider pre-spawn failures too.
	home := testHome(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: preSpawnFailExecutor{
			FailureClass: "auth_refusal",
			Message:      "grok: account mismatch requested=acct-a active=acct-b",
		},
	}
	route := workflowrun.ChildRoute{
		Provider: "grok", Model: "grok-4.5", TaskClass: "tera", Depth: "medium",
		Permission: "bounded_write", AccountRef: "acct-" + strings.Repeat("b", 64),
		InstallRef: "pinst_grok", WindowKind: "weekly", ReservationID: "sres_g",
	}
	res, _ := svc.Execute(context.Background(), withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: "proj-grok-preserve", RunID: "run_grok_preserve",
		Definition:  workflowrun.OneNodeDefinition("g-grok-preserve", "impl"),
		Actor:       "owner",
		ChildRoutes: map[string]workflowrun.ChildRoute{"only": route},
	}))
	if len(res.Children) == 0 {
		t.Fatal("no children")
	}
	c := res.Children[0]
	if c.FailureClass == "route_identity_mismatch" {
		t.Fatalf("grok path must not overwrite: %+v", c)
	}
	if c.FailureClass != "auth_refusal" {
		t.Fatalf("FailureClass=%q want auth_refusal", c.FailureClass)
	}
}
