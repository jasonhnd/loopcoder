package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/orchestration"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/routing"
	"github.com/jasonhnd/loopcoder/internal/runtimepath"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

// NestedChildRouteInput is the CLI-side route request for one nested child.
// Explicit pins come from the immutable child contract and optional global
// --provider/--model/--effort flags.
type NestedChildRouteInput struct {
	RepoPath         string
	ParentRunID      string
	HostProfile      string
	GlobalProvider   string
	GlobalModel      string
	GlobalEffort     string
	HostName         string
	Now              time.Time
	PermissionSafe   func(permission, provider string) error
}

// nestedChildRouteProduction builds durable nested-child task evidence, persists
// a route decision, and returns the exact adapter/model/effort to launch.
// Provider-native nested sub-agents are never selected by this path.
func nestedChildRouteProduction(ctx context.Context, request orchestration.ChildExecutionRequest, input NestedChildRouteInput, decide routeDecideFunc) (orchestration.ChildRouteDecision, error) {
	if decide == nil {
		return orchestration.ChildRouteDecision{}, fmt.Errorf("route decide is not configured")
	}
	now := input.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	permission := strings.TrimSpace(request.Permission)
	switch permission {
	case string(reporter.PermissionReadOnly), string(reporter.PermissionWrite):
	case string(reporter.PermissionOrchestrate):
		return orchestration.ChildRouteDecision{Outcome: routing.RouteOutcomeNoRoute, ZeroProviderLaunches: true},
			fmt.Errorf("orchestrate nested children remain refused")
	default:
		return orchestration.ChildRouteDecision{Outcome: routing.RouteOutcomeNoRoute, ZeroProviderLaunches: true},
			fmt.Errorf("nested child permission %q is not routeable", permission)
	}
	if len(request.Capabilities.Delegation) > 0 {
		return orchestration.ChildRouteDecision{Outcome: routing.RouteOutcomeNoRoute, ZeroProviderLaunches: true},
			fmt.Errorf("provider-native nested delegation remains refused without an approved bridge")
	}

	roots, err := runtimepath.Resolve(ctx, input.RepoPath)
	if err != nil {
		return orchestration.ChildRouteDecision{Outcome: routing.RouteOutcomeNoRoute, ZeroProviderLaunches: true},
			fmt.Errorf("resolve project runtime: %w", err)
	}
	if !roots.Registered || strings.TrimSpace(roots.ProjectID) == "" {
		return orchestration.ChildRouteDecision{Outcome: routing.RouteOutcomeNoRoute, ZeroProviderLaunches: true},
			fmt.Errorf("nested child routing requires a registered project; run loopcoder projects register --repo %s", input.RepoPath)
	}
	if strings.TrimSpace(roots.DatabasePath) == "" {
		return orchestration.ChildRouteDecision{Outcome: routing.RouteOutcomeNoRoute, ZeroProviderLaunches: true},
			fmt.Errorf("nested child routing requires a project database path")
	}
	store, err := storage.Open(ctx, storage.Options{Path: roots.DatabasePath, Now: func() time.Time { return now }})
	if err != nil {
		return orchestration.ChildRouteDecision{Outcome: routing.RouteOutcomeNoRoute, ZeroProviderLaunches: true},
			fmt.Errorf("open project store: %w", err)
	}
	defer store.Close()

	parentRunID := firstNonEmpty(strings.TrimSpace(input.ParentRunID), strings.TrimSpace(request.ParentRunID), strings.TrimSpace(request.RunID))
	hostName := strings.TrimSpace(input.HostName)
	if hostName == "" {
		hostName = "loopcoder-cli"
	}
	actor := delivery.Actor{
		ActorKind:         "system",
		ActorID:           "loopcoder-nested",
		Display:           "loopcoder nested",
		DecisionAuthority: "nested-child-route",
		Source:            "cli.nested",
	}
	host := delivery.Host{
		HostKind:         "cli",
		HostID:           hostName,
		SessionID:        parentRunID,
		ProcessID:        os.Getpid(),
		LoopcoderVersion: "loopcoder",
		Platform:         "darwin/arm64",
	}
	planFP := nestedRouteFingerprint("plan", roots.ProjectID, parentRunID, request.PlanID, request.ChildKey, request.Permission, request.ContractFingerprint)
	authFP := nestedRouteFingerprint("auth", roots.ProjectID, parentRunID, request.PlanID, request.ChildKey, request.Permission)
	inputFP := nestedRouteFingerprint("input", roots.ProjectID, parentRunID, request.PlanID, request.ChildKey, request.Title, request.Work.Instructions)
	policyFP := nestedRouteFingerprint("policy", roots.ProjectID, "nested-child-route", permission)

	runID := "nested-route:" + parentRunID
	run, err := delivery.PersistDeliveryRun(ctx, store, delivery.DeliveryRun{
		DeliveryRunID:            runID,
		ProjectID:                roots.ProjectID,
		State:                    delivery.RunApproved,
		IntentSummary:            fmt.Sprintf("nested child %s (%s)", request.ChildKey, permission),
		InputFingerprint:         inputFP,
		PolicyFingerprint:        policyFP,
		PlanFingerprint:          planFP,
		AuthorizationFingerprint: authFP,
		PolicyVersion:            taskrequirements.PolicyVersion,
		MaxSideEffectClass:       string(taskrequirements.SideEffectProviderLaunch),
		ApprovalStatus:           "approved",
		OverrideStatus:           "none",
		CreatedBy:                actor,
		UpdatedBy:                actor,
		Host:                     host,
	}, delivery.PersistOptions{Now: now, IdempotencyKey: "nested-route-run:" + runID})
	if err != nil {
		return orchestration.ChildRouteDecision{}, fmt.Errorf("persist delivery run for nested route: %w", err)
	}

	taskKey := fmt.Sprintf("nested-child-%s", request.ChildKey)
	sideEffect := taskrequirements.SideEffectProviderLaunch
	requiredOutput := taskrequirements.OutputReport
	roleKey := "worker"
	if permission == string(reporter.PermissionReadOnly) {
		roleKey = "verifier"
		requiredOutput = taskrequirements.OutputVerificationVerdict
		sideEffect = taskrequirements.SideEffectLocalRead
	}
	scopeJSON := `{"allows_provider_launch":true}`
	if permission == string(reporter.PermissionWrite) {
		scopeJSON = `{"allows_repo_write":true,"allows_provider_launch":true}`
	}
	task, err := delivery.PersistTask(ctx, store, delivery.Task{
		ProjectID:                roots.ProjectID,
		DeliveryRunID:            run.DeliveryRunID,
		TaskKey:                  taskKey,
		Title:                    firstNonEmpty(strings.TrimSpace(request.Title), request.ChildKey),
		RequirementsJSON:         fmt.Sprintf(`{"role":%q,"permission":%q,"source":"nested"}`, roleKey, permission),
		ScopeJSON:                scopeJSON,
		Permission:               permission,
		SideEffectClass:          string(sideEffect),
		PolicyVersion:            taskrequirements.PolicyVersion,
		PlanFingerprint:          planFP,
		AuthorizationFingerprint: authFP,
		CreatedBy:                actor,
		UpdatedBy:                actor,
		Host:                     host,
	}, delivery.PersistOptions{Now: now, IdempotencyKey: "nested-route-task:" + run.DeliveryRunID + ":" + taskKey})
	if err != nil {
		return orchestration.ChildRouteDecision{}, fmt.Errorf("persist delivery task for nested route: %w", err)
	}

	req, err := taskrequirements.Classify(taskrequirements.ClassificationInput{
		ProjectID:       roots.ProjectID,
		DeliveryRunID:   run.DeliveryRunID,
		TaskID:          task.TaskID,
		TaskKey:         task.TaskKey,
		Title:           firstNonEmpty(strings.TrimSpace(request.Title), request.ChildKey),
		IntentSummary:   firstNonEmpty(request.Work.Instructions, request.Title, request.ChildKey),
		RoleKey:         roleKey,
		PlanFingerprint: planFP,
		PolicyVersion:   taskrequirements.PolicyVersion,
		RequiredOutput:  requiredOutput,
		Scope: taskrequirements.Scope{
			Issues:               append([]int(nil), request.Scope.Issues...),
			PullRequests:         append([]int(nil), request.Scope.PullRequests...),
			AllowsRepoWrite:      permission == string(reporter.PermissionWrite),
			AllowsProviderLaunch: true,
			Tests:                true,
		},
		CreatedBy: actor,
		Host:      host,
		Now:       now,
	})
	if err != nil {
		return orchestration.ChildRouteDecision{}, fmt.Errorf("classify nested child task requirement: %w", err)
	}
	req.RoleKey = roleKey
	if permission == string(reporter.PermissionReadOnly) {
		req.PermissionRequired = taskrequirements.PermissionReadOnly
		req.SideEffectClass = taskrequirements.SideEffectLocalRead
		req.RequiredOutput = taskrequirements.OutputVerificationVerdict
	} else {
		req.PermissionRequired = taskrequirements.PermissionWrite
		req.SideEffectClass = taskrequirements.SideEffectProviderLaunch
		req.RequiredOutput = taskrequirements.OutputReport
	}
	// Nested children are LoopCoder-managed; do not require provider-native
	// nested-subagent catalog capability.
	req.NestedAllowed = false
	req, err = taskrequirements.PersistTaskRequirement(ctx, store, req, taskrequirements.PersistOptions{Now: now})
	if err != nil {
		return orchestration.ChildRouteDecision{}, fmt.Errorf("persist nested child task requirement: %w", err)
	}

	decisionKey := fmt.Sprintf("nested:%s:child:%s", request.PlanID, request.ChildKey)
	routeRequest := routing.StoredRouteRequest{
		ProjectID:         roots.ProjectID,
		DeliveryRunID:     run.DeliveryRunID,
		TaskRequirementID: req.TaskRequirementID,
		DecisionKey:       decisionKey,
		HostName:          hostName,
		DecidedBy:         actor,
		Host:              host,
		PinActor:          actor,
	}
	pinProvider := firstNonEmpty(
		strings.TrimSpace(request.Work.Provider),
		strings.TrimSpace(request.ProviderDecision.AdapterID),
		strings.TrimSpace(input.GlobalProvider),
	)
	pinModel := firstNonEmpty(
		strings.TrimSpace(request.Work.Model),
		strings.TrimSpace(request.ProviderDecision.ModelCapabilityID),
		strings.TrimSpace(input.GlobalModel),
	)
	pinEffort := firstNonEmpty(
		strings.TrimSpace(request.Work.Effort),
		strings.TrimSpace(request.ProviderDecision.ReasoningProfileID),
		strings.TrimSpace(input.GlobalEffort),
	)
	if pinProvider != "" {
		routeRequest.Pin = &routing.CandidateConstraint{
			AdapterID:            pinProvider,
			ModelCapabilityID:    pinModel,
			InvocationProfileKey: mapEffortToInvocationProfile(pinEffort),
		}
		routeRequest.PinReason = "explicit nested child or --provider pin"
	}

	result, err := decide(ctx, store, routeRequest)
	out := orchestration.ChildRouteDecision{
		RoutingDecisionID:    result.Decision.RoutingDecisionID,
		Outcome:              result.Outcome,
		ChosenReason:         result.Decision.ChosenReason,
		Replayed:             result.Replayed,
		ZeroProviderLaunches: true,
	}
	if err != nil && result.Decision.RoutingDecisionID == "" {
		return out, err
	}
	if result.Outcome == routing.RouteOutcomeNoRoute || result.Decision.DecisionStatus == routing.DecisionStatusNoEligible {
		out.Outcome = routing.RouteOutcomeNoRoute
		if err != nil {
			return out, err
		}
		return out, fmt.Errorf("no_route: no eligible nested child route for %s", decisionKey)
	}
	if err != nil {
		return out, err
	}
	candidate := routing.SelectedCandidate(result.Decision)
	if strings.TrimSpace(candidate.AdapterID) == "" {
		out.Outcome = routing.RouteOutcomeNoRoute
		return out, fmt.Errorf("no_route: selected decision %s has no adapter", result.Decision.RoutingDecisionID)
	}
	adapter := strings.TrimSpace(candidate.AdapterID)
	if input.PermissionSafe != nil {
		if safeErr := input.PermissionSafe(permission, adapter); safeErr != nil {
			out.Outcome = routing.RouteOutcomeNoRoute
			out.ChosenReason = safeErr.Error()
			return out, fmt.Errorf("nested permission gate rejected adapter %q: %w", adapter, safeErr)
		}
	}
	out.AdapterID = adapter
	out.ModelCapabilityID = strings.TrimSpace(candidate.ModelCapabilityID)
	out.Model = firstNonEmpty(candidate.CanonicalModelID, candidate.ModelCapabilityID, pinModel)
	out.Effort = mapInvocationProfileToEffort(candidate.InvocationProfileKey, pinEffort)
	out.ReasoningProfileID = mapEffortToInvocationProfile(out.Effort)
	out.Outcome = routing.RouteOutcomeSelected
	out.ZeroProviderLaunches = false
	return out, nil
}

func nestedRouteFingerprint(kind, projectID string, parts ...any) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "loopcoder.nested.route.%s\n%s\n", kind, projectID)
	for _, part := range parts {
		_, _ = fmt.Fprintf(h, "%v\n", part)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func nestedPermissionSafeAdapter(permission, provider string) error {
	provider = strings.TrimSpace(provider)
	switch strings.TrimSpace(permission) {
	case string(reporter.PermissionReadOnly):
		if !nestedReadOnlyProviderSupported(provider) {
			return fmt.Errorf("provider %q has no registered nested read-only adapter", provider)
		}
		return nil
	case string(reporter.PermissionWrite):
		if !nestedBoundedWriteProviderSupported(provider) {
			return fmt.Errorf("provider %q has no registered nested bounded-write adapter", provider)
		}
		return nil
	default:
		return fmt.Errorf("permission %q has no nested adapter", permission)
	}
}

func nestedChildRouteResolver(opts nestedRunOptions, explicitProvider bool, now func() time.Time) orchestration.ChildRouteResolver {
	if opts.Provider == nestedTestSubprocessProvider {
		return nil
	}
	return func(ctx context.Context, request orchestration.ChildExecutionRequest) (orchestration.ChildRouteDecision, error) {
		// Replay: if the immutable contract already carries a full decision and
		// adapter, re-validate permission safety and reuse without re-scoring.
		if childRouteAlreadyDecided(request) {
			adapter := strings.TrimSpace(request.ProviderDecision.AdapterID)
			if err := nestedPermissionSafeAdapter(request.Permission, adapter); err != nil {
				return orchestration.ChildRouteDecision{
					RoutingDecisionID:    request.ProviderDecision.RoutingDecisionID,
					AdapterID:            adapter,
					Outcome:              routing.RouteOutcomeNoRoute,
					ZeroProviderLaunches: true,
					ChosenReason:         err.Error(),
					Replayed:             true,
				}, err
			}
			return orchestration.ChildRouteDecision{
				RoutingDecisionID:  request.ProviderDecision.RoutingDecisionID,
				AdapterID:          adapter,
				ModelCapabilityID:  request.ProviderDecision.ModelCapabilityID,
				ReasoningProfileID: request.ProviderDecision.ReasoningProfileID,
				Model:              firstNonEmpty(request.Work.Model, request.ProviderDecision.ModelCapabilityID),
				Effort:             firstNonEmpty(request.Work.Effort, request.ProviderDecision.ReasoningProfileID),
				Outcome:            routing.RouteOutcomeSelected,
				Replayed:           true,
			}, nil
		}
		globalProvider := ""
		globalModel := ""
		globalEffort := ""
		if explicitProvider {
			globalProvider = opts.Provider
			globalModel = opts.Model
			globalEffort = opts.Effort
		}
		return nestedChildRouteProduction(ctx, request, NestedChildRouteInput{
			RepoPath:       opts.RepoPath,
			ParentRunID:    request.ParentRunID,
			HostProfile:    opts.HostProfile,
			GlobalProvider: globalProvider,
			GlobalModel:    globalModel,
			GlobalEffort:   globalEffort,
			Now:            now(),
			PermissionSafe: nestedPermissionSafeAdapter,
		}, routing.DecideStoredRoute)
	}
}

func childRouteAlreadyDecided(request orchestration.ChildExecutionRequest) bool {
	return strings.TrimSpace(request.ProviderDecision.RoutingDecisionID) != "" &&
		strings.TrimSpace(request.ProviderDecision.AdapterID) != ""
}
