package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/routing"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

func TestRouteExplainCLIParsesProviderNeutralRequestAndEmitsStrictRedactedJSON(t *testing.T) {
	secret := "sk-" + strings.Repeat("R", 32)
	assignment := "token=" + strings.Repeat("T", 24)
	personalPath := "/Users/example/private/route.txt"
	var captured routing.StoredRouteRequest
	deps := Deps{
		Now: fixedCLINow,
		RouteExplain: func(_ context.Context, _ storage.Store, request routing.StoredRouteRequest) (routing.RouteOperationResult, error) {
			captured = request
			return routeCLIResult(routing.RouteOperationExplain, routing.RouteOutcomeSelected, secret+" "+assignment+" "+personalPath), nil
		},
	}
	var stdout, stderr bytes.Buffer
	code := RunWithDeps([]string{
		"route", "explain", "--db", routeCLICurrentDB(t), "--project-id", "proj-route", "--run-id", "drun-route",
		"--task-requirement-id", "treq-route", "--decision-key", "attempt-1", "--profile", routing.ProfileKeyDeep,
		"--budget-class", " SHORT ", "--deadline-class", " Medium ",
		"--host-name", "codex-cli", "--pin-provider", "grok", "--pin-model", "grok-4.5", "--format", "json",
	}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("route explain exit = %d stderr=%s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("route explain stderr = %q", stderr.String())
	}
	if captured.ProjectID != "proj-route" || captured.DeliveryRunID != "drun-route" || captured.TaskRequirementID != "treq-route" || captured.DecisionKey != "attempt-1" {
		t.Fatalf("captured route identity = %#v", captured)
	}
	if captured.RoutingPolicyProfileKey != routing.ProfileKeyDeep || captured.HostName != "codex-cli" || captured.Pin == nil || captured.Pin.AdapterID != "grok" || captured.Pin.ModelCapabilityID != "grok-4.5" {
		t.Fatalf("captured route policy/pin = %#v", captured)
	}
	if captured.BudgetClass != routing.BudgetClassShort || captured.DeadlineClass != routing.DeadlineClassMedium {
		t.Fatalf("captured route task-fit classes = budget %q deadline %q", captured.BudgetClass, captured.DeadlineClass)
	}
	if captured.DecidedBy.Source != "loopcoder route explain" || captured.Host.ProcessID != 0 || captured.Host.SessionID != "" {
		t.Fatalf("captured route provenance = %#v", captured)
	}
	assertSingleRouteJSON(t, stdout.Bytes(), routing.RouteOperationExplain, routing.RouteOutcomeSelected, 0)
	if output := stdout.String(); strings.Contains(output, secret) || strings.Contains(output, assignment) || strings.Contains(output, personalPath) || !strings.Contains(output, "[REDACTED_") {
		t.Fatalf("route JSON was not redacted: %s", output)
	}
}

func TestRouteDecideCLIRequiresPinReasonAndReturnsTypedNoRoute(t *testing.T) {
	called := false
	deps := Deps{
		Now: fixedCLINow,
		RouteDecide: func(_ context.Context, _ storage.Store, request routing.StoredRouteRequest) (routing.RouteOperationResult, error) {
			called = true
			if request.PinReason != "operator selection" || request.PinActor.ActorID != "owner" {
				t.Fatalf("pin provenance = %#v", request)
			}
			result := routeCLIResult(routing.RouteOperationDecide, routing.RouteOutcomeNoRoute, "no hard-eligible candidates")
			result.Persisted = true
			result.Decision.DecisionStatus = routing.DecisionStatusNoEligible
			result.Decision.TerminalErrorCode = taskrequirements.ErrNoEligibleCandidateCode
			return result, taskrequirements.ErrNoEligibleCandidate
		},
	}
	dbPath := t.TempDir() + "/route.db"
	base := []string{"route", "decide", "--db", dbPath, "--project-id", "proj-route", "--run-id", "drun-route", "--task-requirement-id", "treq-route", "--decision-key", "attempt-2", "--pin-provider", "grok", "--format", "json"}
	var invalidOut, invalidErr bytes.Buffer
	if code := RunWithDeps(base, &invalidOut, &invalidErr, deps); code != 2 {
		t.Fatalf("route decide missing pin reason exit = %d stderr=%s", code, invalidErr.String())
	}
	assertRouteJSONError(t, invalidOut.Bytes(), routing.RouteOperationDecide, "invalid_request")
	if called {
		t.Fatal("route service called for invalid pin provenance")
	}

	args := append(append([]string{}, base...), "--pin-reason", "operator selection", "--actor-id", "owner")
	var stdout, stderr bytes.Buffer
	code := RunWithDeps(args, &stdout, &stderr, deps)
	if code != routeNoRouteExitCode {
		t.Fatalf("route decide no-route exit = %d stderr=%s", code, stderr.String())
	}
	if !called {
		t.Fatal("route decide service was not called")
	}
	assertSingleRouteJSON(t, stdout.Bytes(), routing.RouteOperationDecide, routing.RouteOutcomeNoRoute, 0)
	if !strings.Contains(stderr.String(), string(taskrequirements.ErrNoEligibleCandidateCode)) {
		t.Fatalf("route no-route stderr = %q", stderr.String())
	}
}

func TestRouteJSONRequestErrorsEmitOneTypedEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{
			name: "flag parse",
			args: []string{"route", "explain", "--format=json", "--not-a-route-flag"},
		},
		{
			name: "validation",
			args: []string{"route", "explain", "--format", "json", "--project-id", "proj-route"},
		},
		{
			name: "unexpected argument",
			args: []string{
				"route", "explain", "--format", " JSON ", "--project-id", "proj-route", "--run-id", "drun-route",
				"--task-requirement-id", "treq-route", "--decision-key", "attempt-extra", "unexpected",
			},
		},
		{
			name: "unknown budget class",
			args: []string{
				"route", "explain", "--format", "json", "--project-id", "proj-route", "--run-id", "drun-route",
				"--task-requirement-id", "treq-route", "--decision-key", "attempt-budget", "--budget-class", "tiny",
			},
		},
		{
			name: "unknown deadline class",
			args: []string{
				"route", "explain", "--format", "json", "--project-id", "proj-route", "--run-id", "drun-route",
				"--task-requirement-id", "treq-route", "--decision-key", "attempt-deadline", "--deadline-class", "urgent",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := RunWithDeps(tc.args, &stdout, &stderr, Deps{Now: fixedCLINow}); code != 2 {
				t.Fatalf("route request error exit = %d stderr=%s", code, stderr.String())
			}
			assertRouteJSONError(t, stdout.Bytes(), routing.RouteOperationExplain, "invalid_request")
			if strings.Count(stdout.String(), "\n") != 1 {
				t.Fatalf("route request error emitted multiple records: %q", stdout.String())
			}
		})
	}
}

func TestRouteJSONUnknownSubcommandRedactsUserControlledOperation(t *testing.T) {
	secret := "token=" + strings.Repeat("U", 24)
	var stdout, stderr bytes.Buffer
	if code := RunWithDeps([]string{"route", secret, "--format", "json"}, &stdout, &stderr, Deps{Now: fixedCLINow}); code != 2 {
		t.Fatalf("unknown route subcommand exit = %d stderr=%s", code, stderr.String())
	}
	var envelope routeJSONError
	decodeSingleJSON(t, stdout.Bytes(), &envelope)
	if envelope.Code != "invalid_request" || envelope.ProviderCalls != 0 {
		t.Fatalf("unknown route subcommand envelope = %#v", envelope)
	}
	for _, output := range []string{stdout.String(), stderr.String()} {
		if strings.Contains(output, secret) || !strings.Contains(output, "[REDACTED_") {
			t.Fatalf("unknown route subcommand leaked user input: %s", output)
		}
	}
}

func TestRouteCLIPassesValidTaskFitClassesToServiceForRequirementValidation(t *testing.T) {
	called := false
	deps := Deps{
		Now: fixedCLINow,
		RouteExplain: func(_ context.Context, _ storage.Store, request routing.StoredRouteRequest) (routing.RouteOperationResult, error) {
			called = true
			if request.BudgetClass != routing.BudgetClassVeryShort || request.DeadlineClass != routing.DeadlineClassShort {
				t.Fatalf("route task-fit classes = budget %q deadline %q", request.BudgetClass, request.DeadlineClass)
			}
			return routing.RouteOperationResult{}, &taskrequirements.TypedError{
				Code:    taskrequirements.ErrInvalidRecordCode,
				Message: "budget_class very-short is weaker than task requirement floor medium",
			}
		},
	}
	var stdout, stderr bytes.Buffer
	code := RunWithDeps([]string{
		"route", "explain", "--db", routeCLICurrentDB(t), "--project-id", "proj-route", "--run-id", "drun-route",
		"--task-requirement-id", "treq-route", "--decision-key", "attempt-weaker", "--budget-class", "very-short",
		"--deadline-class", "short", "--format", "json",
	}, &stdout, &stderr, deps)
	if code != 1 {
		t.Fatalf("route service validation exit = %d stderr=%s", code, stderr.String())
	}
	if !called {
		t.Fatal("route service was not called for valid task-fit class syntax")
	}
	assertRouteJSONError(t, stdout.Bytes(), routing.RouteOperationExplain, string(taskrequirements.ErrInvalidRecordCode))
}

func TestRouteCLIEmitsTypedRedactedJSONErrorWithoutProviderCalls(t *testing.T) {
	secret := "ghp_" + strings.Repeat("S", 32)
	deps := Deps{
		Now: fixedCLINow,
		RouteExplain: func(context.Context, storage.Store, routing.StoredRouteRequest) (routing.RouteOperationResult, error) {
			return routing.RouteOperationResult{}, &taskrequirements.TypedError{
				Code: taskrequirements.ErrMissingReferenceCode, Message: "missing route input " + secret + " /Users/example/private",
			}
		},
	}
	var stdout, stderr bytes.Buffer
	code := RunWithDeps([]string{
		"route", "explain", "--db", routeCLICurrentDB(t), "--project-id", "proj-route", "--run-id", "drun-route",
		"--task-requirement-id", "treq-route", "--decision-key", "attempt-error", "--format", "json",
	}, &stdout, &stderr, deps)
	if code != 1 {
		t.Fatalf("route error exit = %d stderr=%s", code, stderr.String())
	}
	var envelope routeJSONError
	decodeSingleJSON(t, stdout.Bytes(), &envelope)
	if envelope.SchemaVersion != RouteFailureSchema || envelope.Code != string(taskrequirements.ErrMissingReferenceCode) || envelope.ProviderCalls != 0 {
		t.Fatalf("route error envelope = %#v", envelope)
	}
	for _, output := range []string{stdout.String(), stderr.String()} {
		if strings.Contains(output, secret) || strings.Contains(output, "/Users/example") || !strings.Contains(output, "[REDACTED_") {
			t.Fatalf("route error was not redacted: %s", output)
		}
	}
}

func TestRouteJSONServiceInvariantAndRenderFailuresEmitOneTypedEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name     string
		result   routing.RouteOperationResult
		wantCode string
	}{
		{name: "empty decision", wantCode: "route_error"},
		{
			name:     "unrenderable decision",
			wantCode: string(taskrequirements.ErrInvalidRecordCode),
			result: func() routing.RouteOperationResult {
				result := routeCLIResult(routing.RouteOperationExplain, routing.RouteOutcomeSelected, "fixture")
				result.Decision.EligibleCandidates = []routing.Candidate{{
					CapabilitySummary: map[string]any{"unsupported": func() {}},
				}}
				return result
			}(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := Deps{
				Now: fixedCLINow,
				RouteExplain: func(context.Context, storage.Store, routing.StoredRouteRequest) (routing.RouteOperationResult, error) {
					return tc.result, nil
				},
			}
			var stdout, stderr bytes.Buffer
			code := RunWithDeps([]string{
				"route", "explain", "--db", routeCLICurrentDB(t), "--project-id", "proj-route", "--run-id", "drun-route",
				"--task-requirement-id", "treq-route", "--decision-key", "attempt-invariant", "--format", "json",
			}, &stdout, &stderr, deps)
			if code != 1 {
				t.Fatalf("route invariant exit = %d stderr=%s", code, stderr.String())
			}
			assertRouteJSONError(t, stdout.Bytes(), routing.RouteOperationExplain, tc.wantCode)
			if strings.Count(stdout.String(), "\n") != 1 {
				t.Fatalf("route invariant emitted multiple records: %q", stdout.String())
			}
		})
	}
}

func TestRouteHelpDocumentsReadOnlyAndPersistedOperations(t *testing.T) {
	var output bytes.Buffer
	printRouteHelp(&output)
	for _, want := range []string{
		"loopcoder route explain", "loopcoder route decide", "without writes", "immutable first route decision",
		"Neither operation launches a provider", "--task-requirement-id", "--budget-class", "--deadline-class", "typed no_route result",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("route help missing %q:\n%s", want, output.String())
		}
	}
}

func routeCLIResult(operation, outcome, reason string) routing.RouteOperationResult {
	return routing.RouteOperationResult{
		SchemaVersion: routing.RouteOperationSchema,
		Operation:     operation,
		Outcome:       outcome,
		Decision: routing.RoutingDecision{
			SchemaVersion: routing.DecisionSchema, RecordVersion: 1, RoutingDecisionID: "rdec_route_cli",
			DecisionKey: "route-cli", DecisionKind: routing.DecisionKindRouting, DecisionStatus: routing.DecisionStatusSelected,
			ProjectID: "proj-route", DeliveryRunID: "drun-route", TaskID: "task-route", TaskRequirementID: "treq-route",
			ChosenCandidateID: "rcand_route_cli", ChosenReason: reason,
		},
	}
}

func routeCLICurrentDB(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/route.db"
	store, err := storage.Open(context.Background(), storage.Options{Path: path, Now: fixedCLINow})
	if err != nil {
		t.Fatalf("seed current route database: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close current route database: %v", err)
	}
	return path
}

func assertSingleRouteJSON(t *testing.T, payload []byte, operation, outcome string, providerCalls int) {
	t.Helper()
	var envelope routeJSONResult
	decodeSingleJSON(t, payload, &envelope)
	if envelope.SchemaVersion != routing.RouteOperationSchema || envelope.Operation != operation || envelope.Outcome != outcome || envelope.ProviderCalls != providerCalls {
		t.Fatalf("route JSON envelope = %#v", envelope)
	}
	if !json.Valid(envelope.Decision) {
		t.Fatalf("route decision is not JSON: %s", envelope.Decision)
	}
}

func assertRouteJSONError(t *testing.T, payload []byte, operation, code string) {
	t.Helper()
	var envelope routeJSONError
	decodeSingleJSON(t, payload, &envelope)
	if envelope.SchemaVersion != RouteFailureSchema || envelope.Operation != operation || envelope.Outcome != "error" || envelope.Code != code || envelope.ProviderCalls != 0 {
		t.Fatalf("route JSON error envelope = %#v", envelope)
	}
}

func decodeSingleJSON(t *testing.T, payload []byte, target any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(target); err != nil {
		t.Fatalf("decode route JSON: %v\npayload=%s", err, payload)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("route output contains a second JSON value: err=%v payload=%s", err, payload)
	}
}
