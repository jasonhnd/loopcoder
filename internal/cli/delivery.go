package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/hostprofile"
	"github.com/jasonhnd/loopcoder/internal/progress"
	"github.com/jasonhnd/loopcoder/internal/routing"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/worker"
)

const (
	deliveryPendingApprovalExitCode = 10
	deliveryStalePlanExitCode       = 11
	deliveryRejectedExitCode        = 12
	deliveryPolicyDeniedExitCode    = 13
	deliveryUnsupportedHostExitCode = 14
	deliveryInterruptedExitCode     = 15
)

func printDeliveryHelp(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  loopcoder delivery plan --project-id <id> --run-id <id> [--format text|json]")
	fmt.Fprintln(w, "  loopcoder delivery decide --project-id <id> --run-id <id> --action approve|reject|edit|expire|supersede --expected-authorization-fingerprint <sha256:...>")
	fmt.Fprintln(w, "  loopcoder delivery continue --project-id <id> --run-id <id> --expected-authorization-fingerprint <sha256:...>")
	fmt.Fprintln(w, "  loopcoder delivery claim-dispatch --project-id <id> --run-id <id> --expected-authorization-fingerprint <sha256:...> [--repo <path>]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Approval-gated DeliveryRun plan, decide, continue, and single-task claim-dispatch operations.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  --db string                         storage database path (default $LOOPCODER_HOME/data/loopcoder.db)")
	fmt.Fprintln(w, "  --project-id string                 project id (required)")
	fmt.Fprintln(w, "  --run-id string                     delivery run id (required)")
	fmt.Fprintln(w, "  --format string                     output format: text or json (default \"text\")")
	fmt.Fprintln(w, "  --action string                     decide action: approve, reject, edit, expire, or supersede")
	fmt.Fprintln(w, "  --expected-authorization-fingerprint string")
	fmt.Fprintln(w, "                                      stale-plan guard from a prior plan response")
	fmt.Fprintln(w, "  --expires-at string                 optional RFC3339 approval expiry for approve")
	fmt.Fprintln(w, "  --actor-id string                   actor id for decision provenance (default \"local-user\")")
	fmt.Fprintln(w, "  --reason string                     optional human decision reason")
	fmt.Fprintln(w, "  --idempotency-key string            caller-stable replay key")
	fmt.Fprintln(w, "  --help                              show help")
}

func runDelivery(args []string, stdout, stderr io.Writer, deps Deps) int {
	if len(args) == 0 {
		printDeliveryHelp(stderr)
		return 2
	}
	if deps.Now == nil {
		deps.Now = DefaultDeps().Now
	}
	switch args[0] {
	case "plan":
		return runDeliveryPlan(args[1:], stdout, stderr, deps)
	case "decide":
		return runDeliveryDecide(args[1:], stdout, stderr, deps)
	case "continue":
		return runDeliveryContinue(args[1:], stdout, stderr, deps)
	case "claim-dispatch":
		return runDeliveryClaimDispatch(args[1:], stdout, stderr, deps)
	default:
		fmt.Fprintf(stderr, "delivery: unknown subcommand %q\n", args[0])
		return 2
	}
}

func runDeliveryPlan(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("delivery plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	common := deliveryFlags{}
	common.bind(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := common.validate("delivery plan"); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	store, closeStore, err := openDeliveryStoreForCLI(context.Background(), common.DBPath, deps)
	if err != nil {
		fmt.Fprintf(stderr, "delivery plan: %v\n", err)
		return 1
	}
	defer closeStore()
	progressRecorder, stopProgress, err := deliveryProgressSupervisorForCLI(store, common.ProjectID, common.RunID, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "delivery plan: progress receipts unavailable: %v\n", err)
		return 1
	}
	defer func() {
		if err := stopProgress(); err != nil {
			fmt.Fprintf(stderr, "delivery plan: progress receipt shutdown: %v\n", err)
		}
	}()
	hostEnforcement := deliveryHostEnforcement()
	proposal, err := delivery.Plan(context.Background(), store, delivery.PlanProposalInput{ProjectID: common.ProjectID, DeliveryRunID: common.RunID, HostEnforcement: hostEnforcement, Progress: progressRecorder})
	if err != nil {
		fmt.Fprintf(stderr, "delivery plan: %v\n", err)
		if common.Format == "json" {
			_ = renderDeliveryError(stdout, "delivery.plan", common.ProjectID, common.RunID, hostEnforcement, err)
		}
		return deliveryExitCode(err)
	}
	return renderDeliveryOutput(stdout, stderr, common.Format, proposal, renderPlanText)
}

func runDeliveryDecide(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("delivery decide", flag.ContinueOnError)
	fs.SetOutput(stderr)
	common := deliveryFlags{}
	common.bind(fs)
	var action, expected, expiresAt, actorID, reason, idempotencyKey, edited string
	fs.StringVar(&action, "action", "", "decision action")
	fs.StringVar(&expected, "expected-authorization-fingerprint", "", "expected authorization fingerprint")
	fs.StringVar(&expiresAt, "expires-at", "", "approval expiry")
	fs.StringVar(&actorID, "actor-id", "local-user", "actor id")
	fs.StringVar(&reason, "reason", "", "decision reason")
	fs.StringVar(&idempotencyKey, "idempotency-key", "", "idempotency key")
	fs.StringVar(&edited, "edited-proposal-json", "", "edited proposal JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := common.validate("delivery decide"); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if strings.TrimSpace(action) == "" {
		fmt.Fprintln(stderr, "delivery decide: --action is required")
		return 2
	}
	store, closeStore, err := openDeliveryStoreForCLI(context.Background(), common.DBPath, deps)
	if err != nil {
		fmt.Fprintf(stderr, "delivery decide: %v\n", err)
		return 1
	}
	defer closeStore()
	progressRecorder, stopProgress, err := deliveryProgressSupervisorForCLI(store, common.ProjectID, common.RunID, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "delivery decide: progress receipts unavailable: %v\n", err)
		return 1
	}
	defer func() {
		if err := stopProgress(); err != nil {
			fmt.Fprintf(stderr, "delivery decide: progress receipt shutdown: %v\n", err)
		}
	}()
	now := deps.Now().UTC()
	hostEnforcement := deliveryHostEnforcement()
	result, err := delivery.Decide(context.Background(), store, delivery.DecisionOptions{
		ProjectID:                        common.ProjectID,
		DeliveryRunID:                    common.RunID,
		Action:                           action,
		ExpectedAuthorizationFingerprint: expected,
		Actor:                            deliveryActor(actorID),
		Host:                             deliveryHost(deps),
		IdempotencyKey:                   idempotencyKey,
		Now:                              now,
		ExpiresAt:                        expiresAt,
		EditedProposalJSON:               edited,
		Reason:                           reason,
		HostEnforcement:                  hostEnforcement,
		Progress:                         progressRecorder,
	})
	if err != nil {
		fmt.Fprintf(stderr, "delivery decide: %v\n", err)
		if common.Format == "json" {
			_ = renderDeliveryError(stdout, "delivery.decide", common.ProjectID, common.RunID, hostEnforcement, err)
		}
		return deliveryExitCode(err)
	}
	return renderDeliveryOutput(stdout, stderr, common.Format, result, renderDecisionText)
}

func runDeliveryContinue(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("delivery continue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	common := deliveryFlags{}
	common.bind(fs)
	var expected, actorID, idempotencyKey string
	fs.StringVar(&expected, "expected-authorization-fingerprint", "", "expected authorization fingerprint")
	fs.StringVar(&actorID, "actor-id", "local-user", "actor id")
	fs.StringVar(&idempotencyKey, "idempotency-key", "", "idempotency key")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := common.validate("delivery continue"); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	store, closeStore, err := openDeliveryStoreForCLI(context.Background(), common.DBPath, deps)
	if err != nil {
		fmt.Fprintf(stderr, "delivery continue: %v\n", err)
		return 1
	}
	defer closeStore()
	progressRecorder, stopProgress, err := deliveryProgressSupervisorForCLI(store, common.ProjectID, common.RunID, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "delivery continue: progress receipts unavailable: %v\n", err)
		return 1
	}
	defer func() {
		if err := stopProgress(); err != nil {
			fmt.Fprintf(stderr, "delivery continue: progress receipt shutdown: %v\n", err)
		}
	}()
	hostEnforcement := deliveryHostEnforcement()
	result, err := delivery.Continue(context.Background(), store, delivery.ContinueOptions{
		ProjectID:                        common.ProjectID,
		DeliveryRunID:                    common.RunID,
		ExpectedAuthorizationFingerprint: expected,
		Actor:                            deliveryActor(actorID),
		Host:                             deliveryHost(deps),
		IdempotencyKey:                   idempotencyKey,
		Now:                              deps.Now().UTC(),
		HostEnforcement:                  hostEnforcement,
		Progress:                         progressRecorder,
	})
	if err != nil {
		fmt.Fprintf(stderr, "delivery continue: %v\n", err)
		if common.Format == "json" {
			_ = renderDeliveryError(stdout, "delivery.continue", common.ProjectID, common.RunID, hostEnforcement, err)
		}
		return deliveryExitCode(err)
	}
	return renderDeliveryOutput(stdout, stderr, common.Format, result, renderContinueText)
}

func runDeliveryClaimDispatch(args []string, stdout, stderr io.Writer, deps Deps) int {
	if deps.Dispatch == nil {
		deps.Dispatch = DefaultDeps().Dispatch
	}
	if deps.Now == nil {
		deps.Now = DefaultDeps().Now
	}
	fs := flag.NewFlagSet("delivery claim-dispatch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	common := deliveryFlags{}
	common.bind(fs)
	var expected, actorID, idempotencyKey, repoPath, provider, model, effort string
	fs.StringVar(&expected, "expected-authorization-fingerprint", "", "expected authorization fingerprint")
	fs.StringVar(&actorID, "actor-id", "local-user", "actor id")
	fs.StringVar(&idempotencyKey, "idempotency-key", "", "idempotency key")
	fs.StringVar(&repoPath, "repo", "", "repository path for worker dispatch")
	fs.StringVar(&provider, "provider", "", "optional explicit worker provider pin")
	fs.StringVar(&model, "model", "", "optional worker model pin")
	fs.StringVar(&effort, "effort", "", "optional worker effort pin")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := common.validate("delivery claim-dispatch"); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if strings.TrimSpace(repoPath) == "" {
		fmt.Fprintln(stderr, "delivery claim-dispatch: --repo is required to dispatch a claimed task")
		return 2
	}
	resolvedRepo, err := resolveRepo(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "delivery claim-dispatch: %v\n", err)
		return 1
	}
	store, closeStore, err := openDeliveryStoreForCLI(context.Background(), common.DBPath, deps)
	if err != nil {
		fmt.Fprintf(stderr, "delivery claim-dispatch: %v\n", err)
		return 1
	}
	defer closeStore()
	progressRecorder, stopProgress, err := deliveryProgressSupervisorForCLI(store, common.ProjectID, common.RunID, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "delivery claim-dispatch: progress receipts unavailable: %v\n", err)
		return 1
	}
	defer func() {
		if err := stopProgress(); err != nil {
			fmt.Fprintf(stderr, "delivery claim-dispatch: progress receipt shutdown: %v\n", err)
		}
	}()
	hostEnforcement := deliveryHostEnforcement()
	claim, err := delivery.ClaimOneReadyTask(context.Background(), store, delivery.ClaimDispatchOptions{
		ProjectID:                        common.ProjectID,
		DeliveryRunID:                    common.RunID,
		ExpectedAuthorizationFingerprint: expected,
		Actor:                            deliveryActor(actorID),
		Host:                             deliveryHost(deps),
		IdempotencyKey:                   idempotencyKey,
		ExecutorID:                       "loopcoder-cli-claim-dispatch",
		Now:                              deps.Now().UTC(),
		HostEnforcement:                  hostEnforcement,
		Progress:                         progressRecorder,
	})
	if err != nil {
		fmt.Fprintf(stderr, "delivery claim-dispatch: %v\n", err)
		if common.Format == "json" {
			_ = renderDeliveryError(stdout, "delivery.claim-dispatch", common.ProjectID, common.RunID, hostEnforcement, err)
		}
		return deliveryExitCode(err)
	}
	type claimDispatchReceipt struct {
		delivery.ClaimDispatchResult
		RoutingDecisionID string `json:"routing_decision_id,omitempty"`
		Provider          string `json:"provider,omitempty"`
		Model             string `json:"model,omitempty"`
		Effort            string `json:"effort,omitempty"`
		WorkerRunID       string `json:"worker_run_id,omitempty"`
		WorkerDispatched  bool   `json:"worker_dispatched"`
		ProviderInvoked   bool   `json:"provider_invoked,omitempty"`
	}
	receipt := claimDispatchReceipt{ClaimDispatchResult: claim}
	if !claim.Claimed {
		return renderDeliveryOutput(stdout, stderr, common.Format, receipt, func(w io.Writer, r claimDispatchReceipt) error {
			fmt.Fprintf(w, "delivery claim-dispatch: no ready task\n")
			fmt.Fprintf(w, "project_id: %s\n", r.ProjectID)
			fmt.Fprintf(w, "delivery_run_id: %s\n", r.DeliveryRunID)
			fmt.Fprintf(w, "outcome: %s\n", r.Outcome)
			fmt.Fprintf(w, "next_action: %s\n", r.NextAction)
			return nil
		})
	}
	if claim.IssueNumber <= 0 {
		fmt.Fprintln(stderr, "delivery claim-dispatch: claimed task has no issue number for worker dispatch")
		return 1
	}

	// Route through persisted worker decision, then launch exactly once.
	resolveRoute := deps.ResolveWorkerDispatchRoute
	if resolveRoute == nil && deps.RouteDecide != nil {
		decide := deps.RouteDecide
		resolveRoute = func(ctx context.Context, input WorkerDispatchRouteInput) (WorkerDispatchRouteResult, error) {
			return resolveWorkerDispatchRouteProduction(ctx, input, decide)
		}
	}
	workerOpts := worker.Options{
		RepoPath:    resolvedRepo,
		IssueNumber: claim.IssueNumber,
		IssueTitle:  firstNonEmptyCLI(claim.TaskTitle, fmt.Sprintf("Delivery task %s", claim.TaskKey)),
		RunID:       claim.DeliveryRunID + ":" + claim.AttemptID,
		Attempt:     int(claim.ClaimGeneration),
		Provider:    provider,
		Model:       model,
		Effort:      effort,
		Stderr:      stderr,
	}
	if resolveRoute != nil {
		routeResult, routeErr := resolveRoute(context.Background(), WorkerDispatchRouteInput{
			RepoPath:         resolvedRepo,
			RunID:            workerOpts.RunID,
			IssueNumber:      workerOpts.IssueNumber,
			IssueTitle:       workerOpts.IssueTitle,
			Attempt:          workerOpts.Attempt,
			ExplicitProvider: strings.TrimSpace(provider),
			ExplicitModel:    strings.TrimSpace(model),
			ExplicitEffort:   strings.TrimSpace(effort),
			HostName:         "loopcoder-cli",
			Now:              deps.Now(),
		})
		if routeErr != nil {
			if routeResult.Outcome == routing.RouteOutcomeNoRoute || strings.Contains(routeErr.Error(), "no_route") {
				fmt.Fprintf(stderr, "delivery claim-dispatch: no_route after claim: %v\n", routeErr)
				receipt.RoutingDecisionID = routeResult.RoutingDecisionID
				receipt.NextAction = "claimed task has no eligible worker route; inspect inventory and policy"
				if common.Format == "json" {
					_ = json.NewEncoder(stdout).Encode(receipt)
				}
				return 20
			}
			fmt.Fprintf(stderr, "delivery claim-dispatch: route decide: %v\n", routeErr)
			return 1
		}
		workerOpts.Provider = routeResult.Provider
		workerOpts.Model = routeResult.Model
		workerOpts.Effort = routeResult.Effort
		workerOpts.RoutingDecisionID = routeResult.RoutingDecisionID
		receipt.RoutingDecisionID = routeResult.RoutingDecisionID
		receipt.Provider = routeResult.Provider
		receipt.Model = routeResult.Model
		receipt.Effort = routeResult.Effort
	} else if strings.TrimSpace(provider) == "" {
		fmt.Fprintln(stderr, "delivery claim-dispatch: unpinned worker requires route decide; pass --provider or production RouteDecide deps")
		return 2
	} else {
		workerOpts.Provider = provider
		workerOpts.Model = model
		workerOpts.Effort = effort
		receipt.Provider = provider
		receipt.Model = model
		receipt.Effort = effort
	}

	result, dispatchErr := deps.Dispatch(context.Background(), workerOpts)
	result = applyTypedFallbackAfterDispatch(context.Background(), workerOpts.RepoPath, workerOpts, result, deps.Now, stderr)
	receipt.WorkerDispatched = true
	receipt.WorkerRunID = result.RunID
	receipt.ProviderInvoked = result.ProviderInvoked
	if dispatchErr != nil {
		fmt.Fprintf(stderr, "delivery claim-dispatch: worker dispatch: %v\n", dispatchErr)
		if strings.TrimSpace(result.NextAction) != "" {
			receipt.NextAction = result.NextAction
		} else {
			receipt.NextAction = "inspect claimed attempt and recover ambiguous launch; do not claim another task automatically"
		}
		if common.Format == "json" {
			_ = json.NewEncoder(stdout).Encode(receipt)
		}
		return 1
	}
	receipt.NextAction = "monitor worker run and continue delivery after terminal outcome"
	return renderDeliveryOutput(stdout, stderr, common.Format, receipt, func(w io.Writer, r claimDispatchReceipt) error {
		fmt.Fprintf(w, "delivery claim-dispatch: claimed and dispatched one task\n")
		fmt.Fprintf(w, "project_id: %s\n", r.ProjectID)
		fmt.Fprintf(w, "delivery_run_id: %s\n", r.DeliveryRunID)
		fmt.Fprintf(w, "task_id: %s\n", r.TaskID)
		fmt.Fprintf(w, "attempt_id: %s\n", r.AttemptID)
		fmt.Fprintf(w, "issue: %d\n", r.IssueNumber)
		fmt.Fprintf(w, "routing_decision_id: %s\n", r.RoutingDecisionID)
		fmt.Fprintf(w, "provider: %s\n", r.Provider)
		fmt.Fprintf(w, "worker_run_id: %s\n", r.WorkerRunID)
		fmt.Fprintf(w, "next_action: %s\n", r.NextAction)
		return nil
	})
}

type deliveryFlags struct {
	DBPath    string
	ProjectID string
	RunID     string
	Format    string
}

func (f *deliveryFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&f.DBPath, "db", "", "storage database path")
	fs.StringVar(&f.ProjectID, "project-id", "", "project id")
	fs.StringVar(&f.RunID, "run-id", "", "delivery run id")
	fs.StringVar(&f.Format, "format", "text", "output format")
}

func (f deliveryFlags) validate(label string) error {
	if strings.TrimSpace(f.ProjectID) == "" {
		return fmt.Errorf("%s: --project-id is required", label)
	}
	if strings.TrimSpace(f.RunID) == "" {
		return fmt.Errorf("%s: --run-id is required", label)
	}
	switch f.Format {
	case "text", "json":
		return nil
	default:
		return fmt.Errorf("%s: invalid --format %q; want text or json", label, f.Format)
	}
}

func openDeliveryStoreForCLI(ctx context.Context, dbPath string, deps Deps) (storage.Store, func(), error) {
	dbPath = strings.TrimSpace(dbPath)
	if dbPath == "" {
		layout, err := home.Resolve(home.Deps{Getenv: os.Getenv, UserHomeDir: os.UserHomeDir})
		if err != nil {
			return nil, nil, err
		}
		dbPath = layout.DatabasePath()
	}
	store, err := storage.Open(ctx, storage.Options{Path: dbPath, Now: deps.Now})
	if err != nil {
		return nil, nil, err
	}
	return store, func() { _ = store.Close() }, nil
}

func deliveryProgressSupervisorForCLI(store storage.Store, projectID, runID string, diagnostics io.Writer) (progress.Recorder, func() error, error) {
	return progressSupervisorForStore(store, projectID, runID, diagnostics)
}

func renderDeliveryOutput[T any](stdout, stderr io.Writer, format string, value T, text func(io.Writer, T) error) int {
	if format == "json" {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(value); err != nil {
			fmt.Fprintf(stderr, "delivery: write output: %v\n", err)
			return 1
		}
		return 0
	}
	if err := text(stdout, value); err != nil {
		fmt.Fprintf(stderr, "delivery: write output: %v\n", err)
		return 1
	}
	return 0
}

func renderPlanText(w io.Writer, proposal delivery.PlanProposal) error {
	_, err := fmt.Fprintf(w, "DELIVERY PLAN\nrun: %s\nstate: %s\napproval: %s\nfingerprint: %s\noperation: %s\nside_effect_class: %s\npermission: %s\ntasks: %d\nedges: %d\n",
		proposal.DeliveryRunID, proposal.RunState, proposal.ApprovalRequirement, proposal.AuthorizationFingerprint,
		proposal.Invocation.Contract.Operation, proposal.Invocation.Contract.SideEffectClass, proposal.Invocation.Contract.Permission,
		proposal.TaskCount, proposal.EdgeCount)
	return err
}

func renderDecisionText(w io.Writer, result delivery.DecisionResult) error {
	_, err := fmt.Fprintf(w, "DELIVERY DECISION\nrun: %s\naction: %s\noutcome: %s\nstate: %s\napproval_status: %s\nfingerprint: %s\noperation: %s\nside_effect_class: %s\npermission: %s\n",
		result.DeliveryRunID, result.Action, result.Outcome, result.RunState, result.ApprovalStatus, result.AuthorizationFingerprint,
		result.Invocation.Contract.Operation, result.Invocation.Contract.SideEffectClass, result.Invocation.Contract.Permission)
	return err
}

func renderContinueText(w io.Writer, result delivery.ContinueResult) error {
	_, err := fmt.Fprintf(w, "DELIVERY CONTINUE\nrun: %s\noutcome: %s\nstate: %s\napproval_status: %s\nfingerprint: %s\noperation: %s\nside_effect_class: %s\npermission: %s\n",
		result.DeliveryRunID, result.Outcome, result.RunState, result.ApprovalStatus, result.AuthorizationFingerprint,
		result.Invocation.Contract.Operation, result.Invocation.Contract.SideEffectClass, result.Invocation.Contract.Permission)
	return err
}

func deliveryExitCode(err error) int {
	switch {
	case errors.Is(err, delivery.ErrApprovalRequired), errors.Is(err, delivery.ErrExpiredApproval):
		return deliveryPendingApprovalExitCode
	case errors.Is(err, delivery.ErrStaleApproval):
		return deliveryStalePlanExitCode
	case errors.Is(err, delivery.ErrPolicyDenied):
		return deliveryPolicyDeniedExitCode
	case errors.Is(err, delivery.ErrUnsupportedHostCapability):
		return deliveryUnsupportedHostExitCode
	case errors.Is(err, delivery.ErrInvocationInterrupted):
		return deliveryInterruptedExitCode
	case errors.Is(err, delivery.ErrInvalidTransition):
		if strings.Contains(err.Error(), "rejected") {
			return deliveryRejectedExitCode
		}
		return 1
	default:
		return 1
	}
}

type deliveryTerminalOutcome struct {
	SchemaVersion string                      `json:"schema_version"`
	Operation     string                      `json:"operation"`
	ProjectID     string                      `json:"project_id"`
	DeliveryRunID string                      `json:"delivery_run_id"`
	Outcome       string                      `json:"outcome"`
	ErrorCode     string                      `json:"error_code"`
	ErrorMessage  string                      `json:"error_message"`
	Invocation    delivery.InvocationEvidence `json:"invocation"`
}

func renderDeliveryError(w io.Writer, operation, projectID, runID string, enforcement delivery.HostEnforcement, err error) error {
	invocation, _ := delivery.ContractForOperation(operation)
	code := ""
	var typed *delivery.TypedError
	if errors.As(err, &typed) {
		code = string(typed.Code)
	}
	if code == "" {
		code = "ErrDeliveryInvocation"
	}
	outcome := "failed"
	switch {
	case errors.Is(err, delivery.ErrUnsupportedHostCapability):
		outcome = delivery.OutcomeUnsupported
	case errors.Is(err, delivery.ErrInvocationInterrupted):
		outcome = delivery.OutcomeInterrupted
	case errors.Is(err, delivery.ErrStaleApproval):
		outcome = delivery.OutcomeStale
	case errors.Is(err, delivery.ErrPolicyDenied):
		outcome = delivery.OutcomeDeclined
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(deliveryTerminalOutcome{
		SchemaVersion: "loopcoder.delivery_terminal_outcome.v1",
		Operation:     operation,
		ProjectID:     projectID,
		DeliveryRunID: runID,
		Outcome:       outcome,
		ErrorCode:     code,
		ErrorMessage:  err.Error(),
		Invocation: delivery.InvocationEvidence{
			Contract:    invocation,
			Enforcement: enforcement,
		},
	})
}

func deliveryActor(actorID string) delivery.Actor {
	actorID = strings.TrimSpace(actorID)
	if actorID == "" {
		actorID = "local-user"
	}
	return delivery.Actor{ActorKind: "user", ActorID: actorID, Display: actorID, DecisionAuthority: "user", Source: "cli"}
}

func deliveryHost(deps Deps) delivery.Host {
	return delivery.Host{
		HostKind:         "cli",
		HostID:           runtime.GOOS + "-" + runtime.GOARCH,
		SessionID:        "local-cli",
		ProcessID:        os.Getpid(),
		LoopcoderVersion: normalizeBuildInfo(deps.BuildInfo).Version,
		Platform:         runtime.GOOS + "-" + runtime.GOARCH,
	}
}

func deliveryHostEnforcement() delivery.HostEnforcement {
	resolved, err := hostprofile.Resolve(hostprofile.Options{Getenv: os.Getenv})
	if err != nil {
		return delivery.UnsupportedHostEnforcement(err.Error())
	}
	enforcement := delivery.SupportedHostEnforcement(resolved.Name, string(resolved.Source))
	enforcement.Cancellation = resolved.Runtime.SupportsCancel
	enforcement.StableJSON = resolved.Runtime.SupportsJSONOutput
	enforcement.Stdout = resolved.Runtime.PreservesStdout
	enforcement.Stderr = resolved.Runtime.PreservesStderr
	return enforcement
}
