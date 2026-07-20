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
	"github.com/jasonhnd/loopcoder/internal/routing"
	"github.com/jasonhnd/loopcoder/internal/runtimepath"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

// WorkerDispatchRouteInput is the dispatch-side route request for an ordinary
// Worker launch. Explicit provider/model/effort flags become route pins.
type WorkerDispatchRouteInput struct {
	RepoPath         string
	RunID            string
	IssueNumber      int
	IssueTitle       string
	IssueBody        string
	Attempt          int
	ExplicitProvider string
	ExplicitModel    string
	ExplicitEffort   string
	HostName         string
	Now              time.Time
}

// WorkerDispatchRouteResult is the launch-facing selection from a route decide.
type WorkerDispatchRouteResult struct {
	Provider             string
	Model                string
	Effort               string
	RoutingDecisionID    string
	DecisionKey          string
	Outcome              string
	ChosenReason         string
	Replayed             bool
	TaskRequirementID    string
	DeliveryRunID        string
	ProjectID            string
	ZeroProviderLaunches bool
}

// resolveWorkerDispatchRouteProduction builds durable worker task evidence,
// persists a route decision, and returns the exact adapter/model/effort to launch.
// Unpinned work never falls back to an implicit Codex default.
func resolveWorkerDispatchRouteProduction(ctx context.Context, input WorkerDispatchRouteInput, decide routeDecideFunc) (WorkerDispatchRouteResult, error) {
	if decide == nil {
		return WorkerDispatchRouteResult{}, fmt.Errorf("route decide is not configured")
	}
	now := input.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if input.IssueNumber <= 0 {
		return WorkerDispatchRouteResult{}, fmt.Errorf("issue number is required for worker route decision")
	}
	if strings.TrimSpace(input.IssueTitle) == "" {
		return WorkerDispatchRouteResult{}, fmt.Errorf("issue title is required for worker route decision")
	}
	roots, err := runtimepath.Resolve(ctx, input.RepoPath)
	if err != nil {
		return WorkerDispatchRouteResult{}, fmt.Errorf("resolve project runtime: %w", err)
	}
	if !roots.Registered || strings.TrimSpace(roots.ProjectID) == "" {
		return WorkerDispatchRouteResult{}, fmt.Errorf("worker routing requires a registered project; run loopcoder projects register --repo %s", input.RepoPath)
	}
	if strings.TrimSpace(roots.DatabasePath) == "" {
		return WorkerDispatchRouteResult{}, fmt.Errorf("worker routing requires a project database path")
	}
	store, err := storage.Open(ctx, storage.Options{Path: roots.DatabasePath, Now: func() time.Time { return now }})
	if err != nil {
		return WorkerDispatchRouteResult{}, fmt.Errorf("open project store: %w", err)
	}
	defer store.Close()

	runID := strings.TrimSpace(input.RunID)
	if runID == "" {
		runID = state.RunIDForIssue(input.IssueNumber, now)
	}
	attempt := input.Attempt
	if attempt <= 0 {
		attempt = 1
	}
	hostName := strings.TrimSpace(input.HostName)
	if hostName == "" {
		hostName = "loopcoder-cli"
	}
	actor := delivery.Actor{
		ActorKind:         "system",
		ActorID:           "loopcoder-dispatch",
		Display:           "loopcoder dispatch",
		DecisionAuthority: "router",
		Source:            "cli.dispatch",
	}
	// Policy pin/exclusion rows require ActorKind=user (validatePolicyInput).
	pinActor := delivery.Actor{
		ActorKind:         "user",
		ActorID:           "local-user",
		Display:           "local user",
		DecisionAuthority: "user",
		Source:            "cli.dispatch",
	}
	host := delivery.Host{
		HostKind:         "cli",
		HostID:           hostName,
		SessionID:        runID,
		ProcessID:        os.Getpid(),
		LoopcoderVersion: "loopcoder",
		Platform:         "darwin/arm64",
	}
	planFP := dispatchRouteFingerprint("plan", roots.ProjectID, runID, input.IssueNumber, input.IssueTitle, input.IssueBody)
	authFP := dispatchRouteFingerprint("auth", roots.ProjectID, runID, input.IssueNumber, input.IssueTitle, input.IssueBody)
	inputFP := dispatchRouteFingerprint("input", roots.ProjectID, runID, input.IssueNumber, input.IssueTitle, input.IssueBody)
	policyFP := dispatchRouteFingerprint("policy", roots.ProjectID, "worker-dispatch")

	run, err := delivery.PersistDeliveryRun(ctx, store, delivery.DeliveryRun{
		DeliveryRunID:            runID,
		ProjectID:                roots.ProjectID,
		State:                    delivery.RunApproved,
		IntentSummary:            fmt.Sprintf("dispatch worker for issue #%d: %s", input.IssueNumber, strings.TrimSpace(input.IssueTitle)),
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
	}, delivery.PersistOptions{Now: now, IdempotencyKey: "dispatch-route-run:" + runID})
	if err != nil {
		return WorkerDispatchRouteResult{}, fmt.Errorf("persist delivery run for route: %w", err)
	}

	taskKey := fmt.Sprintf("worker-issue-%d", input.IssueNumber)
	task, err := delivery.PersistTask(ctx, store, delivery.Task{
		ProjectID:                roots.ProjectID,
		DeliveryRunID:            run.DeliveryRunID,
		TaskKey:                  taskKey,
		Title:                    strings.TrimSpace(input.IssueTitle),
		RequirementsJSON:         `{"role":"worker","source":"dispatch"}`,
		ScopeJSON:                fmt.Sprintf(`{"issues":[%d],"allows_repo_write":true,"allows_provider_launch":true}`, input.IssueNumber),
		Permission:               string(taskrequirements.PermissionWrite),
		SideEffectClass:          string(taskrequirements.SideEffectProviderLaunch),
		PolicyVersion:            taskrequirements.PolicyVersion,
		PlanFingerprint:          planFP,
		AuthorizationFingerprint: authFP,
		CreatedBy:                actor,
		UpdatedBy:                actor,
		Host:                     host,
	}, delivery.PersistOptions{Now: now, IdempotencyKey: "dispatch-route-task:" + run.DeliveryRunID + ":" + taskKey})
	if err != nil {
		return WorkerDispatchRouteResult{}, fmt.Errorf("persist delivery task for route: %w", err)
	}

	req, err := taskrequirements.Classify(taskrequirements.ClassificationInput{
		ProjectID:       roots.ProjectID,
		DeliveryRunID:   run.DeliveryRunID,
		TaskID:          task.TaskID,
		TaskKey:         task.TaskKey,
		Title:           input.IssueTitle,
		IntentSummary:   input.IssueBody,
		RoleKey:         "worker",
		PlanFingerprint: planFP,
		PolicyVersion:   taskrequirements.PolicyVersion,
		RequiredOutput:  taskrequirements.OutputPR,
		Scope: taskrequirements.Scope{
			Issues:               []int{input.IssueNumber},
			AllowsRepoWrite:      true,
			AllowsProviderLaunch: true,
			Tests:                true,
		},
		CreatedBy: actor,
		Host:      host,
		Now:       now,
	})
	if err != nil {
		return WorkerDispatchRouteResult{}, fmt.Errorf("classify worker task requirement: %w", err)
	}
	req, err = taskrequirements.PersistTaskRequirement(ctx, store, req, taskrequirements.PersistOptions{Now: now})
	if err != nil {
		return WorkerDispatchRouteResult{}, fmt.Errorf("persist worker task requirement: %w", err)
	}

	decisionKey := fmt.Sprintf("worker:issue-%d:attempt-%d", input.IssueNumber, attempt)
	request := routing.StoredRouteRequest{
		ProjectID:         roots.ProjectID,
		DeliveryRunID:     run.DeliveryRunID,
		TaskRequirementID: req.TaskRequirementID,
		DecisionKey:       decisionKey,
		HostName:          hostName,
		DecidedBy:         actor,
		Host:              host,
		PinActor:          pinActor,
	}
	if pinProvider := strings.TrimSpace(input.ExplicitProvider); pinProvider != "" {
		request.Pin = &routing.CandidateConstraint{
			AdapterID:            pinProvider,
			ModelCapabilityID:    strings.TrimSpace(input.ExplicitModel),
			InvocationProfileKey: mapEffortToInvocationProfile(input.ExplicitEffort),
		}
		request.PinReason = "explicit dispatch --provider pin"
	}

	result, err := decide(ctx, store, request)
	if err != nil && result.Decision.RoutingDecisionID == "" {
		return WorkerDispatchRouteResult{}, err
	}
	out := WorkerDispatchRouteResult{
		RoutingDecisionID:    result.Decision.RoutingDecisionID,
		DecisionKey:          decisionKey,
		Outcome:              result.Outcome,
		ChosenReason:         result.Decision.ChosenReason,
		Replayed:             result.Replayed,
		TaskRequirementID:    req.TaskRequirementID,
		DeliveryRunID:        run.DeliveryRunID,
		ProjectID:            roots.ProjectID,
		ZeroProviderLaunches: true,
	}
	if result.Outcome == routing.RouteOutcomeNoRoute || result.Decision.DecisionStatus == routing.DecisionStatusNoEligible {
		out.Outcome = routing.RouteOutcomeNoRoute
		if err != nil {
			return out, err
		}
		return out, fmt.Errorf("no_route: no eligible worker route for decision %s", decisionKey)
	}
	if err != nil {
		return out, err
	}
	candidate := routing.SelectedCandidate(result.Decision)
	if strings.TrimSpace(candidate.AdapterID) == "" {
		out.Outcome = routing.RouteOutcomeNoRoute
		return out, fmt.Errorf("no_route: selected decision %s has no adapter", result.Decision.RoutingDecisionID)
	}
	out.Provider = candidate.AdapterID
	out.Model = firstNonEmpty(candidate.CanonicalModelID, candidate.ModelCapabilityID)
	out.Effort = mapInvocationProfileToEffort(candidate.InvocationProfileKey, input.ExplicitEffort)
	out.Outcome = routing.RouteOutcomeSelected
	out.ZeroProviderLaunches = false
	return out, nil
}

type routeDecideFunc func(context.Context, storage.Store, routing.StoredRouteRequest) (routing.RouteOperationResult, error)

func dispatchRouteFingerprint(kind, projectID string, parts ...any) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "loopcoder.dispatch.route.%s\n%s\n", kind, projectID)
	for _, part := range parts {
		_, _ = fmt.Fprintf(h, "%v\n", part)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func mapEffortToInvocationProfile(effort string) string {
	switch strings.TrimSpace(strings.ToLower(effort)) {
	case "":
		return ""
	case "xhigh", "max", "deep":
		return "deep"
	case "high", "standard", "default", "medium":
		return "default"
	case "low", "fast", "minimal":
		return "fast"
	default:
		return strings.TrimSpace(effort)
	}
}

func mapInvocationProfileToEffort(profile, explicit string) string {
	if e := strings.TrimSpace(explicit); e != "" {
		return e
	}
	switch strings.TrimSpace(strings.ToLower(profile)) {
	case "deep":
		return "xhigh"
	case "fast":
		return "low"
	case "default", "":
		return "high"
	default:
		return profile
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
