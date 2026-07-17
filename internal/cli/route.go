package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/routing"
	"github.com/jasonhnd/loopcoder/internal/sanitize"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

const (
	routeNoRouteExitCode = 20
	routeOutputMaxBytes  = 1024 * 1024
)

func printRouteHelp(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  loopcoder route explain --project-id <id> --run-id <id> --task-requirement-id <id> --decision-key <key> [flags]")
	fmt.Fprintln(w, "  loopcoder route decide  --project-id <id> --run-id <id> --task-requirement-id <id> --decision-key <key> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Explain a provider-neutral route without writes, or persist one immutable first route decision.")
	fmt.Fprintln(w, "Role, permission, capability, task-fit, budget, deadline, health, and quota inputs are loaded from durable local records.")
	fmt.Fprintln(w, "Neither operation launches a provider or refreshes provider telemetry.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  --db string                       storage database path (default $LOOPCODER_HOME/data/loopcoder.db)")
	fmt.Fprintln(w, "  --project-id string               project id (required)")
	fmt.Fprintln(w, "  --run-id string                   delivery run id (required)")
	fmt.Fprintln(w, "  --task-requirement-id string      immutable task requirement id (required)")
	fmt.Fprintln(w, "  --decision-key string             stable execution-attempt route key (required)")
	fmt.Fprintln(w, "  --profile string                  fast-v1, balanced-v1, or deep-v1 (default \"balanced-v1\")")
	fmt.Fprintln(w, "  --host-name string                runtime capability host name (default \"generic-local\")")
	fmt.Fprintln(w, "  --pin-provider string             optional adapter id constraint")
	fmt.Fprintln(w, "  --pin-installation string         optional provider installation id constraint")
	fmt.Fprintln(w, "  --pin-account string              optional account profile id constraint")
	fmt.Fprintln(w, "  --pin-model string                optional model capability id constraint")
	fmt.Fprintln(w, "  --pin-invocation-profile string   optional invocation profile key constraint")
	fmt.Fprintln(w, "  --pin-reason string               required provenance reason when decide persists a pin")
	fmt.Fprintln(w, "  --actor-id string                 user provenance for a persisted pin (default \"local-user\")")
	fmt.Fprintln(w, "  --format string                   output format: text or json (default \"text\")")
	fmt.Fprintln(w, "  --help                            show help")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Exit codes:")
	fmt.Fprintln(w, "  0   selected route")
	fmt.Fprintln(w, "  1   typed route or storage failure")
	fmt.Fprintln(w, "  2   invalid command-line request")
	fmt.Fprintf(w, "  %d  typed no_route result; no provider was launched\n", routeNoRouteExitCode)
}

func runRoute(args []string, stdout, stderr io.Writer, deps Deps) int {
	if len(args) == 0 {
		printRouteHelp(stderr)
		return 2
	}
	if deps.Now == nil {
		deps.Now = DefaultDeps().Now
	}
	switch args[0] {
	case routing.RouteOperationExplain:
		return runRouteOperation(routing.RouteOperationExplain, args[1:], stdout, stderr, deps)
	case routing.RouteOperationDecide:
		return runRouteOperation(routing.RouteOperationDecide, args[1:], stdout, stderr, deps)
	default:
		fmt.Fprintf(stderr, "route: unknown subcommand %q\n", args[0])
		return 2
	}
}

type routeFlags struct {
	DBPath            string
	ProjectID         string
	RunID             string
	TaskRequirementID string
	DecisionKey       string
	ProfileKey        string
	HostName          string
	PinProvider       string
	PinInstallation   string
	PinAccount        string
	PinModel          string
	PinInvocation     string
	PinReason         string
	ActorID           string
	Format            string
}

func (f *routeFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&f.DBPath, "db", "", "storage database path")
	fs.StringVar(&f.ProjectID, "project-id", "", "project id")
	fs.StringVar(&f.RunID, "run-id", "", "delivery run id")
	fs.StringVar(&f.TaskRequirementID, "task-requirement-id", "", "task requirement id")
	fs.StringVar(&f.DecisionKey, "decision-key", "", "stable route decision key")
	fs.StringVar(&f.ProfileKey, "profile", routing.ProfileKeyBalanced, "routing policy profile key")
	fs.StringVar(&f.HostName, "host-name", "generic-local", "runtime capability host name")
	fs.StringVar(&f.PinProvider, "pin-provider", "", "pinned adapter id")
	fs.StringVar(&f.PinInstallation, "pin-installation", "", "pinned provider installation id")
	fs.StringVar(&f.PinAccount, "pin-account", "", "pinned account profile id")
	fs.StringVar(&f.PinModel, "pin-model", "", "pinned model capability id")
	fs.StringVar(&f.PinInvocation, "pin-invocation-profile", "", "pinned invocation profile key")
	fs.StringVar(&f.PinReason, "pin-reason", "", "pin provenance reason")
	fs.StringVar(&f.ActorID, "actor-id", "local-user", "pin actor id")
	fs.StringVar(&f.Format, "format", "text", "output format")
}

func (f *routeFlags) normalize() {
	f.DBPath = strings.TrimSpace(f.DBPath)
	f.ProjectID = strings.TrimSpace(f.ProjectID)
	f.RunID = strings.TrimSpace(f.RunID)
	f.TaskRequirementID = strings.TrimSpace(f.TaskRequirementID)
	f.DecisionKey = strings.TrimSpace(f.DecisionKey)
	f.ProfileKey = strings.TrimSpace(f.ProfileKey)
	f.HostName = strings.TrimSpace(f.HostName)
	f.PinProvider = strings.TrimSpace(f.PinProvider)
	f.PinInstallation = strings.TrimSpace(f.PinInstallation)
	f.PinAccount = strings.TrimSpace(f.PinAccount)
	f.PinModel = strings.TrimSpace(f.PinModel)
	f.PinInvocation = strings.TrimSpace(f.PinInvocation)
	f.PinReason = strings.TrimSpace(f.PinReason)
	f.ActorID = strings.TrimSpace(f.ActorID)
	f.Format = strings.ToLower(strings.TrimSpace(f.Format))
}

func (f routeFlags) validate(operation string) error {
	for _, field := range []struct{ name, value string }{
		{name: "--project-id", value: f.ProjectID},
		{name: "--run-id", value: f.RunID},
		{name: "--task-requirement-id", value: f.TaskRequirementID},
		{name: "--decision-key", value: f.DecisionKey},
	} {
		if field.value == "" {
			return fmt.Errorf("route %s: %s is required", operation, field.name)
		}
	}
	if f.Format != "text" && f.Format != "json" {
		return fmt.Errorf("route %s: invalid --format %q; want text or json", operation, f.Format)
	}
	switch f.ProfileKey {
	case routing.ProfileKeyFast, routing.ProfileKeyBalanced, routing.ProfileKeyDeep:
	default:
		return fmt.Errorf("route %s: invalid --profile %q; want fast-v1, balanced-v1, or deep-v1", operation, f.ProfileKey)
	}
	for _, field := range []struct{ name, value string }{
		{name: "project id", value: f.ProjectID},
		{name: "run id", value: f.RunID},
		{name: "task requirement id", value: f.TaskRequirementID},
		{name: "decision key", value: f.DecisionKey},
		{name: "profile", value: f.ProfileKey},
		{name: "host name", value: f.HostName},
		{name: "pin provider", value: f.PinProvider},
		{name: "pin installation", value: f.PinInstallation},
		{name: "pin account", value: f.PinAccount},
		{name: "pin model", value: f.PinModel},
		{name: "pin invocation profile", value: f.PinInvocation},
		{name: "actor id", value: f.ActorID},
	} {
		if field.value != "" && sanitize.Text(field.value) != field.value {
			return fmt.Errorf("route %s: %s contains a credential, personal path, or control character", operation, field.name)
		}
	}
	if !f.hasPin() && f.PinReason != "" {
		return fmt.Errorf("route %s: --pin-reason requires an explicit pin constraint", operation)
	}
	if operation == routing.RouteOperationDecide && f.hasPin() && f.PinReason == "" {
		return fmt.Errorf("route decide: --pin-reason is required with an explicit pin")
	}
	return nil
}

func (f routeFlags) hasPin() bool {
	return f.PinProvider != "" || f.PinInstallation != "" || f.PinAccount != "" || f.PinModel != "" || f.PinInvocation != ""
}

func (f routeFlags) request(operation string, deps Deps) routing.StoredRouteRequest {
	request := routing.StoredRouteRequest{
		ProjectID:               f.ProjectID,
		DeliveryRunID:           f.RunID,
		TaskRequirementID:       f.TaskRequirementID,
		DecisionKey:             f.DecisionKey,
		RoutingPolicyProfileKey: f.ProfileKey,
		HostName:                f.HostName,
		PinReason:               f.PinReason,
		PinActor:                deliveryActor(f.ActorID),
		DecidedBy: delivery.Actor{
			ActorKind: "system", ActorID: "route-cli", DecisionAuthority: "router", Source: "loopcoder route " + operation,
		},
		Host: routeHost(deps),
	}
	if f.hasPin() {
		request.Pin = &routing.CandidateConstraint{
			AdapterID: f.PinProvider, ProviderInstallationID: f.PinInstallation, AccountProfileID: f.PinAccount,
			ModelCapabilityID: f.PinModel, InvocationProfileKey: f.PinInvocation,
		}
	}
	return request
}

func routeHost(deps Deps) delivery.Host {
	platform := runtime.GOOS + "-" + runtime.GOARCH
	return delivery.Host{
		HostKind: "cli", HostID: platform, LoopcoderVersion: normalizeBuildInfo(deps.BuildInfo).Version, Platform: platform,
	}
}

func runRouteOperation(operation string, args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("route "+operation, flag.ContinueOnError)
	fs.SetOutput(stderr)
	flags := routeFlags{}
	flags.bind(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "route %s: unexpected argument %q\n", operation, fs.Arg(0))
		return 2
	}
	flags.normalize()
	if err := flags.validate(operation); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	store, closeStore, err := openDeliveryStoreForCLI(context.Background(), flags.DBPath, deps)
	if err != nil {
		return renderRouteFailure(stdout, stderr, flags.Format, operation, err)
	}
	defer closeStore()

	request := flags.request(operation, deps)
	var result routing.RouteOperationResult
	if operation == routing.RouteOperationExplain {
		fn := deps.RouteExplain
		if fn == nil {
			fn = routing.ExplainStoredRoute
		}
		result, err = fn(context.Background(), store, request)
	} else {
		fn := deps.RouteDecide
		if fn == nil {
			fn = routing.DecideStoredRoute
		}
		result, err = fn(context.Background(), store, request)
	}
	if result.Decision.RoutingDecisionID != "" {
		if renderErr := renderRouteResult(stdout, flags.Format, result); renderErr != nil {
			fmt.Fprintf(stderr, "route %s: %s\n", operation, sanitize.Text(renderErr.Error()))
			return 1
		}
		if err != nil {
			fmt.Fprintf(stderr, "route %s: %s\n", operation, sanitize.Text(err.Error()))
		}
		if result.Outcome == routing.RouteOutcomeNoRoute {
			return routeNoRouteExitCode
		}
		if err != nil {
			return 1
		}
		return 0
	}
	if err != nil {
		return renderRouteFailure(stdout, stderr, flags.Format, operation, err)
	}
	fmt.Fprintf(stderr, "route %s: route service returned no decision\n", operation)
	return 1
}

type routeJSONResult struct {
	SchemaVersion          string          `json:"schema_version"`
	Operation              string          `json:"operation"`
	Outcome                string          `json:"outcome"`
	Persisted              bool            `json:"persisted"`
	Replayed               bool            `json:"replayed"`
	ProviderCalls          int             `json:"provider_calls"`
	PriorRoutingDecisionID string          `json:"prior_routing_decision_id,omitempty"`
	Decision               json.RawMessage `json:"decision"`
}

func renderRouteResult(w io.Writer, format string, result routing.RouteOperationResult) error {
	decisionJSON, err := routing.ExplainJSON(result.Decision)
	if err != nil {
		return err
	}
	if format == "json" {
		payload, err := json.Marshal(routeJSONResult{
			SchemaVersion: result.SchemaVersion, Operation: result.Operation, Outcome: result.Outcome,
			Persisted: result.Persisted, Replayed: result.Replayed, ProviderCalls: result.ProviderCalls,
			PriorRoutingDecisionID: result.PriorRoutingDecisionID,
			Decision:               json.RawMessage(decisionJSON),
		})
		if err != nil {
			return err
		}
		if len(payload)+1 > routeOutputMaxBytes {
			return fmt.Errorf("route JSON exceeds %d-byte output ceiling", routeOutputMaxBytes)
		}
		_, err = w.Write(append(payload, '\n'))
		return err
	}
	text := fmt.Sprintf("ROUTE %s\noutcome: %s\npersisted: %t\nreplayed: %t\nprovider_calls: %d\n%s\n",
		strings.ToUpper(result.Operation), result.Outcome, result.Persisted, result.Replayed, result.ProviderCalls, routing.ExplainHuman(result.Decision))
	text = sanitize.Text(text)
	if len(text) > routeOutputMaxBytes {
		return fmt.Errorf("route text exceeds %d-byte output ceiling", routeOutputMaxBytes)
	}
	_, err = io.WriteString(w, text)
	return err
}

type routeJSONError struct {
	SchemaVersion string `json:"schema_version"`
	Operation     string `json:"operation"`
	Outcome       string `json:"outcome"`
	Code          string `json:"code"`
	Message       string `json:"message"`
	ProviderCalls int    `json:"provider_calls"`
}

func renderRouteFailure(stdout, stderr io.Writer, format, operation string, err error) int {
	message := sanitize.Text(err.Error())
	code := routeErrorCode(err)
	if format == "json" {
		payload, marshalErr := json.Marshal(routeJSONError{
			SchemaVersion: RouteFailureSchema, Operation: operation, Outcome: "error", Code: code, Message: message, ProviderCalls: 0,
		})
		if marshalErr == nil && len(payload)+1 <= routeOutputMaxBytes {
			_, _ = stdout.Write(append(payload, '\n'))
		}
	}
	fmt.Fprintf(stderr, "route %s: %s\n", operation, message)
	return 1
}

const RouteFailureSchema = "loopcoder.route_error.v1"

func routeErrorCode(err error) string {
	var requirementErr *taskrequirements.TypedError
	if errors.As(err, &requirementErr) {
		return string(requirementErr.Code)
	}
	var deliveryErr *delivery.TypedError
	if errors.As(err, &deliveryErr) {
		return string(deliveryErr.Code)
	}
	return "route_error"
}
