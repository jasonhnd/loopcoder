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
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

// VerifierDispatchRouteInput selects an independent read-only verifier.
type VerifierDispatchRouteInput struct {
	RepoPath         string
	PRNumber         int
	WorkerProvider   string
	WorkerModel      string
	ExplicitProvider string
	ExplicitModel    string
	ExplicitEffort   string
	HostName         string
	Now              time.Time
}

// VerifierDispatchRouteResult is the launch-facing verifier selection.
type VerifierDispatchRouteResult struct {
	Provider          string
	Model             string
	Effort            string
	RoutingDecisionID string
	DecisionKey       string
	Outcome           string
	ChosenReason      string
	Replayed          bool
	ProjectID         string
	DeliveryRunID     string
}

// resolveVerifierDispatchRouteProduction builds a verifier TaskRequirement,
// persists a route decision, and returns the selected read-only verifier.
// When no independent candidate exists, outcome is no_route / needs-human.
func resolveVerifierDispatchRouteProduction(ctx context.Context, input VerifierDispatchRouteInput, decide routeDecideFunc) (VerifierDispatchRouteResult, error) {
	if decide == nil {
		return VerifierDispatchRouteResult{}, fmt.Errorf("route decide is not configured")
	}
	now := input.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if input.PRNumber <= 0 {
		return VerifierDispatchRouteResult{}, fmt.Errorf("pr number is required for verifier route decision")
	}
	roots, err := runtimepath.Resolve(ctx, input.RepoPath)
	if err != nil {
		return VerifierDispatchRouteResult{}, fmt.Errorf("resolve project runtime: %w", err)
	}
	if !roots.Registered || strings.TrimSpace(roots.ProjectID) == "" {
		return VerifierDispatchRouteResult{}, fmt.Errorf("verifier routing requires a registered project; run loopcoder projects register --repo %s", input.RepoPath)
	}
	store, err := storage.Open(ctx, storage.Options{Path: roots.DatabasePath, Now: func() time.Time { return now }})
	if err != nil {
		return VerifierDispatchRouteResult{}, fmt.Errorf("open project store: %w", err)
	}
	defer store.Close()

	runID := fmt.Sprintf("loopreview-pr-%d", input.PRNumber)
	hostName := strings.TrimSpace(input.HostName)
	if hostName == "" {
		if strings.TrimSpace(os.Getenv("PASEO_AGENT_ID")) != "" {
			hostName = "paseo-style"
		} else {
			hostName = "generic-local"
		}
	}
	actor := delivery.Actor{
		ActorKind:         "system",
		ActorID:           "loopcoder-loopreview",
		Display:           "loopcoder loopreview",
		DecisionAuthority: "router",
		Source:            "cli.loopreview",
	}
	pinActor := delivery.Actor{
		ActorKind:         "user",
		ActorID:           "local-user",
		Display:           "local user",
		DecisionAuthority: "user",
		Source:            "cli.loopreview",
	}
	host := delivery.Host{
		HostKind:         "cli",
		HostID:           hostName,
		SessionID:        runID,
		ProcessID:        os.Getpid(),
		LoopcoderVersion: "loopcoder",
		Platform:         "darwin/arm64",
	}
	planFP := verifierRouteFingerprint("plan", roots.ProjectID, runID, input.PRNumber, input.WorkerProvider, input.WorkerModel)
	authFP := verifierRouteFingerprint("auth", roots.ProjectID, runID, input.PRNumber)
	inputFP := verifierRouteFingerprint("input", roots.ProjectID, runID, input.PRNumber)
	policyFP := verifierRouteFingerprint("policy", roots.ProjectID, "verifier-dispatch")

	run, err := delivery.PersistDeliveryRun(ctx, store, delivery.DeliveryRun{
		DeliveryRunID:            runID,
		ProjectID:                roots.ProjectID,
		State:                    delivery.RunApproved,
		IntentSummary:            fmt.Sprintf("independent verifier for PR #%d", input.PRNumber),
		InputFingerprint:         inputFP,
		PolicyFingerprint:        policyFP,
		PlanFingerprint:          planFP,
		AuthorizationFingerprint: authFP,
		PolicyVersion:            taskrequirements.PolicyVersion,
		MaxSideEffectClass:       string(taskrequirements.SideEffectLocalRead),
		ApprovalStatus:           "approved",
		OverrideStatus:           "none",
		CreatedBy:                actor,
		UpdatedBy:                actor,
		Host:                     host,
	}, delivery.PersistOptions{Now: now, IdempotencyKey: "verifier-route-run:" + runID})
	if err != nil {
		return VerifierDispatchRouteResult{}, fmt.Errorf("persist delivery run for verifier route: %w", err)
	}

	taskKey := fmt.Sprintf("verifier-pr-%d", input.PRNumber)
	task, err := delivery.PersistTask(ctx, store, delivery.Task{
		ProjectID:                roots.ProjectID,
		DeliveryRunID:            run.DeliveryRunID,
		TaskKey:                  taskKey,
		Title:                    fmt.Sprintf("Verify PR #%d", input.PRNumber),
		RequirementsJSON:         `{"role":"verifier","permission":"read-only"}`,
		ScopeJSON:                fmt.Sprintf(`{"pull_requests":[%d],"allows_provider_launch":true}`, input.PRNumber),
		Permission:               string(taskrequirements.PermissionReadOnly),
		SideEffectClass:          string(taskrequirements.SideEffectLocalRead),
		PolicyVersion:            taskrequirements.PolicyVersion,
		PlanFingerprint:          planFP,
		AuthorizationFingerprint: authFP,
		CreatedBy:                actor,
		UpdatedBy:                actor,
		Host:                     host,
	}, delivery.PersistOptions{Now: now, IdempotencyKey: "verifier-route-task:" + run.DeliveryRunID + ":" + taskKey})
	if err != nil {
		return VerifierDispatchRouteResult{}, fmt.Errorf("persist delivery task for verifier route: %w", err)
	}

	req, err := taskrequirements.Classify(taskrequirements.ClassificationInput{
		ProjectID:       roots.ProjectID,
		DeliveryRunID:   run.DeliveryRunID,
		TaskID:          task.TaskID,
		TaskKey:         task.TaskKey,
		Title:           fmt.Sprintf("Verify PR #%d", input.PRNumber),
		IntentSummary:   fmt.Sprintf("Independent read-only verification of PR #%d; worker was %s/%s", input.PRNumber, input.WorkerProvider, input.WorkerModel),
		RoleKey:         "verifier",
		PlanFingerprint: planFP,
		PolicyVersion:   taskrequirements.PolicyVersion,
		RequiredOutput:  taskrequirements.OutputVerificationVerdict,
		Scope: taskrequirements.Scope{
			PullRequests:         []int{input.PRNumber},
			AllowsProviderLaunch: true,
			Tests:                true,
		},
		CreatedBy: actor,
		Host:      host,
		Now:       now,
	})
	if err != nil {
		return VerifierDispatchRouteResult{}, fmt.Errorf("classify verifier task requirement: %w", err)
	}
	// Force verifier permission/read-only surface regardless of classifier floor.
	req.RoleKey = "verifier"
	req.PermissionRequired = taskrequirements.PermissionReadOnly
	req.RequiredOutput = taskrequirements.OutputVerificationVerdict
	req.SideEffectClass = taskrequirements.SideEffectLocalRead
	if strings.TrimSpace(input.WorkerProvider) != "" {
		// Persist worker exclusion so independence policy can reject same identity.
		// Hard exclusion via policy pin of a different adapter is insufficient when
		// inventory has only one provider; route will then return no_route.
	}
	req, err = taskrequirements.PersistTaskRequirement(ctx, store, req, taskrequirements.PersistOptions{Now: now})
	if err != nil {
		return VerifierDispatchRouteResult{}, fmt.Errorf("persist verifier task requirement: %w", err)
	}

	decisionKey := fmt.Sprintf("verifier:pr-%d", input.PRNumber)
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
	if pin := strings.TrimSpace(input.ExplicitProvider); pin != "" {
		request.Pin = &routing.CandidateConstraint{
			AdapterID:            pin,
			ModelCapabilityID:    strings.TrimSpace(input.ExplicitModel),
			InvocationProfileKey: mapEffortToInvocationProfile(input.ExplicitEffort),
		}
		request.PinReason = "explicit loopreview --provider pin"
	}

	result, err := decide(ctx, store, request)
	if err != nil && result.Decision.RoutingDecisionID == "" {
		return VerifierDispatchRouteResult{}, err
	}
	out := VerifierDispatchRouteResult{
		RoutingDecisionID: result.Decision.RoutingDecisionID,
		DecisionKey:       decisionKey,
		Outcome:           result.Outcome,
		ChosenReason:      result.Decision.ChosenReason,
		Replayed:          result.Replayed,
		ProjectID:         roots.ProjectID,
		DeliveryRunID:     run.DeliveryRunID,
	}
	if result.Outcome == routing.RouteOutcomeNoRoute || result.Decision.DecisionStatus == routing.DecisionStatusNoEligible {
		out.Outcome = routing.RouteOutcomeNoRoute
		if err != nil {
			return out, err
		}
		return out, fmt.Errorf("no_route: no independent read-only verifier for PR #%d", input.PRNumber)
	}
	if err != nil {
		return out, err
	}
	candidate := routing.SelectedCandidate(result.Decision)
	if strings.TrimSpace(candidate.AdapterID) == "" {
		out.Outcome = routing.RouteOutcomeNoRoute
		return out, fmt.Errorf("no_route: selected verifier decision has no adapter")
	}
	// Fail closed if selected verifier is the same adapter as the worker.
	if worker := strings.TrimSpace(input.WorkerProvider); worker != "" && strings.EqualFold(candidate.AdapterID, worker) {
		out.Outcome = routing.RouteOutcomeNoRoute
		return out, fmt.Errorf("no_route: verifier %q is not independent of worker %q", candidate.AdapterID, worker)
	}
	out.Provider = candidate.AdapterID
	out.Model = firstNonEmpty(candidate.CanonicalModelID, candidate.ModelCapabilityID)
	out.Effort = mapInvocationProfileToEffort(candidate.InvocationProfileKey, input.ExplicitEffort)
	out.Outcome = routing.RouteOutcomeSelected
	return out, nil
}

func verifierRouteFingerprint(kind, projectID string, parts ...any) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "loopcoder.verifier.route.%s\n%s\n", kind, projectID)
	for _, part := range parts {
		_, _ = fmt.Fprintf(h, "%v\n", part)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
