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
	personalPath := "/Users/example/private/route.txt"
	var captured routing.StoredRouteRequest
	deps := Deps{
		Now: fixedCLINow,
		RouteExplain: func(_ context.Context, _ storage.Store, request routing.StoredRouteRequest) (routing.RouteOperationResult, error) {
			captured = request
			return routeCLIResult(routing.RouteOperationExplain, routing.RouteOutcomeSelected, secret+" "+personalPath), nil
		},
	}
	var stdout, stderr bytes.Buffer
	code := RunWithDeps([]string{
		"route", "explain", "--db", t.TempDir() + "/route.db", "--project-id", "proj-route", "--run-id", "drun-route",
		"--task-requirement-id", "treq-route", "--decision-key", "attempt-1", "--profile", routing.ProfileKeyDeep,
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
	if captured.DecidedBy.Source != "loopcoder route explain" || captured.Host.ProcessID != 0 || captured.Host.SessionID != "" {
		t.Fatalf("captured route provenance = %#v", captured)
	}
	assertSingleRouteJSON(t, stdout.Bytes(), routing.RouteOperationExplain, routing.RouteOutcomeSelected, 0)
	if output := stdout.String(); strings.Contains(output, secret) || strings.Contains(output, personalPath) || !strings.Contains(output, "[REDACTED_") {
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
		"route", "explain", "--db", t.TempDir() + "/route.db", "--project-id", "proj-route", "--run-id", "drun-route",
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

func TestRouteHelpDocumentsReadOnlyAndPersistedOperations(t *testing.T) {
	var output bytes.Buffer
	printRouteHelp(&output)
	for _, want := range []string{
		"loopcoder route explain", "loopcoder route decide", "without writes", "immutable first route decision",
		"Neither operation launches a provider", "--task-requirement-id", "typed no_route result",
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
