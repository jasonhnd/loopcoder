package orchestration

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/guardrails"
	"github.com/jasonhnd/loopcoder/internal/progress"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

const (
	NestedStatusSucceeded                     = "succeeded"
	NestedStatusSucceededWithOptionalFailures = "succeeded_with_optional_failures"
	NestedStatusFailed                        = "failed"
	NestedStatusNeedsHuman                    = "needs-human"
	NestedStatusBlocked                       = "blocked"
	NestedStatusLost                          = "lost"
	NestedStatusSkipped                       = "skipped"
	NestedStatusCancelled                     = "cancelled"
	NestedStatusTimedOut                      = "timed_out"
	NestedStatusAbandoned                     = "abandoned"
	NestedStatusQueued                        = "queued"
	NestedStatusRunning                       = "running"
	NestedStatusWaiting                       = "waiting"

	NestedEventChildQueued   = "nested.child.queued"
	NestedEventChildRunning  = "nested.child.running"
	NestedEventChildFinished = "nested.child.finished"
	NestedEventParentDone    = "nested.parent.finished"

	NestedOutcomeReadOnlyPolicyViolation   = "read_only_policy_violation"
	NestedOutcomeWriteScopePolicyViolation = "write_scope_policy_violation"

	ReplayActionNew     = "new"
	ReplayActionReused  = "reused"
	ReplayActionResumed = "resumed"
	ReplayActionRetried = "retried"
	ReplayActionBlocked = "blocked"

	nestedClaimCleanupTimeout = 5 * time.Second
)

var nestedExecutorSequence uint64
var (
	nestedClaimLeaseDuration = 30 * time.Minute
	nestedClaimRenewEvery    = nestedClaimLeaseDuration / 3
)

type ChildRunExecutor func(ctx context.Context, request ChildExecutionRequest) (ChildRunResult, error)
type RecordNestedEventFunc func(repoPath, runID string, event state.Event) error
type TaskBoundaryRouteReevaluationFunc func(context.Context, TaskBoundaryRouteReevaluationEvent) error

type TaskBoundaryRouteReevaluationEvent struct {
	ParentRunID string
	ChildRunID  string
	ChildKey    string
	Status      string
	FinishedAt  time.Time
}

type NestedScheduleOptions struct {
	RepoPath         string
	PlanID           string
	RootRunID        string
	ParentRunID      string
	BaseBranch       string
	ParentDepth      int
	MaxDepth         int
	MaxChildren      int
	ConcurrencyLimit int
	Children         []ChildRunPlan
	Budget           config.GuardrailBudget
	CircuitBreaker   config.GuardrailCircuitBreaker
	Now              time.Time
	Clock            func() time.Time
	RuntimeClock     func() time.Time
	Plan             *ChildPlan
	Store            storage.Store
	Progress         progress.Recorder
	// AllowUnbudgetedLocalTest is reserved for the CLI's deterministic
	// test-subprocess provider. Real provider routes must leave it false.
	AllowUnbudgetedLocalTest bool
	// NativeBridge is the only authority that can enable provider-native child
	// execution. No production bridge is registered in v0.8.1.
	NativeBridge ProviderNativeBridge

	// ResolveChildRoute, when set, selects a provider/model for each unpinned
	// child from the immutable execution contract before plan persistence and
	// claim/launch. Replay reuses the durable decision via the resolver.
	ResolveChildRoute ChildRouteResolver

	Execute                       ChildRunExecutor
	RecordEvent                   RecordNestedEventFunc
	TaskBoundaryRouteReevaluation TaskBoundaryRouteReevaluationFunc
}

type ChildRunPlan struct {
	ID           string           `json:"id,omitempty"`
	ChildKey     string           `json:"child_key,omitempty"`
	Title        string           `json:"title,omitempty"`
	Role         string           `json:"role,omitempty"`
	RunID        string           `json:"run_id,omitempty"`
	ProviderKey  string           `json:"provider_idempotency_key,omitempty"`
	Issue        int              `json:"issue,omitempty"`
	ScopeIssues  []int            `json:"scope_issues,omitempty"`
	Scope        ChildScope       `json:"scope"`
	Permission   string           `json:"permission"`
	DependsOn    []string         `json:"depends_on"`
	Aggregation  ChildAggregation `json:"aggregation"`
	Required     bool             `json:"required,omitempty"`
	Optional     bool             `json:"optional,omitempty"`
	Ordinal      int              `json:"ordinal,omitempty"`
	Depth        int              `json:"depth,omitempty"`
	Metadata     json.RawMessage  `json:"metadata,omitempty"`
	ReplayAction string           `json:"-"`
}

type NestedScheduleReport struct {
	Version            int                       `json:"version"`
	RepoPath           string                    `json:"repo_path"`
	BaseBranch         string                    `json:"base_branch"`
	ParentRunID        string                    `json:"parent_run_id"`
	Status             string                    `json:"status"`
	Outcome            string                    `json:"outcome,omitempty"`
	StartedAt          string                    `json:"started_at"`
	FinishedAt         string                    `json:"finished_at"`
	ConcurrencyLimit   int                       `json:"concurrency_limit"`
	Children           []ChildRunResult          `json:"children"`
	Summary            NestedSummary             `json:"summary"`
	ExecutorCapability *NestedExecutorCapability `json:"executor_capability,omitempty"`
	Refusals           []NestedPermissionRefusal `json:"refusals,omitempty"`
}

type ChildRunResult struct {
	ID                  string                          `json:"id"`
	ChildKey            string                          `json:"child_key,omitempty"`
	Title               string                          `json:"title,omitempty"`
	Role                string                          `json:"role,omitempty"`
	RunID               string                          `json:"run_id"`
	Issue               int                             `json:"issue,omitempty"`
	Scope               ChildScope                      `json:"scope"`
	Permission          string                          `json:"permission"`
	DependsOn           []string                        `json:"depends_on,omitempty"`
	Aggregation         ChildAggregation                `json:"aggregation"`
	Required            bool                            `json:"required,omitempty"`
	Optional            bool                            `json:"optional,omitempty"`
	Ordinal             int                             `json:"ordinal"`
	Depth               int                             `json:"depth"`
	Status              string                          `json:"status"`
	Outcome             string                          `json:"outcome,omitempty"`
	ReplayAction        string                          `json:"replay_action,omitempty"`
	ClaimOutcome        string                          `json:"claim_outcome,omitempty"`
	ClaimOwner          string                          `json:"claim_owner,omitempty"`
	ClaimGeneration     int64                           `json:"claim_generation,omitempty"`
	LeaseExpiresAt      string                          `json:"lease_expires_at,omitempty"`
	ClaimPhase          string                          `json:"claim_phase,omitempty"`
	ProviderKey         string                          `json:"provider_idempotency_key,omitempty"`
	ProviderReceipt     string                          `json:"provider_receipt,omitempty"`
	ContractSchema      string                          `json:"execution_contract_schema,omitempty"`
	ContractFingerprint string                          `json:"execution_contract_fingerprint,omitempty"`
	StartedAt           string                          `json:"started_at,omitempty"`
	FinishedAt          string                          `json:"finished_at,omitempty"`
	Error               string                          `json:"error,omitempty"`
	Reason              string                          `json:"reason,omitempty"`
	NextAction          string                          `json:"next_action,omitempty"`
	AttemptPath         string                          `json:"attempt_path,omitempty"`
	RecoveryContextPath string                          `json:"recovery_context_path,omitempty"`
	Report              *reporter.Report                `json:"report,omitempty"`
	ReadOnlyEnforcement *state.ReadOnlyEnforcementAudit `json:"read_only_enforcement,omitempty"`
	MutationManifest    *state.MutationManifestAudit    `json:"mutation_manifest,omitempty"`
	WorktreePath        string                          `json:"worktree_path,omitempty"`
	RoutingDecisionID   string                          `json:"routing_decision_id,omitempty"`
	RouteAdapterID      string                          `json:"route_adapter_id,omitempty"`
	RouteModel          string                          `json:"route_model,omitempty"`
	RouteOutcome        string                          `json:"route_outcome,omitempty"`
	RouteReason         string                          `json:"route_reason,omitempty"`
	RouteReplayed       bool                            `json:"route_replayed,omitempty"`
}

type NestedSummary struct {
	RequiredCount   int `json:"required_count"`
	OptionalCount   int `json:"optional_count"`
	SucceededCount  int `json:"succeeded_count"`
	FailedCount     int `json:"failed_count"`
	NeedsHumanCount int `json:"needs_human_count"`
	SkippedCount    int `json:"skipped_count"`
	CancelledCount  int `json:"cancelled_count"`
}

func ScheduleNestedRuns(ctx context.Context, opts NestedScheduleOptions) (NestedScheduleReport, error) {
	if opts.Execute == nil {
		return NestedScheduleReport{}, fmt.Errorf("child run executor is required")
	}
	if strings.TrimSpace(opts.RepoPath) == "" {
		opts.RepoPath = "."
	}
	if strings.TrimSpace(opts.BaseBranch) == "" {
		opts.BaseBranch = lcdefaults.BaseBranch
	}
	if opts.ConcurrencyLimit <= 0 {
		opts.ConcurrencyLimit = lcdefaults.NestedSchedulerMaxConcurrency
	}
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = lcdefaults.NestedSchedulerMaxDepth
	}
	if opts.MaxChildren <= 0 {
		opts.MaxChildren = lcdefaults.NestedSchedulerMaxChildren
	}
	if opts.MaxDepth > NestedHardMaxDepth {
		return NestedScheduleReport{}, fmt.Errorf("max depth %d exceeds hard maximum %d", opts.MaxDepth, NestedHardMaxDepth)
	}
	if opts.RecordEvent == nil {
		opts.RecordEvent = state.AppendEvent
	}
	clock := opts.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	opts.Clock = clock
	runtimeClock := opts.RuntimeClock
	if runtimeClock == nil {
		runtimeClock = clock
	}
	started := opts.Now
	if started.IsZero() {
		started = clock()
	}
	started = started.UTC()
	if strings.TrimSpace(opts.ParentRunID) == "" {
		opts.ParentRunID = state.RunIDForWave(started)
	}
	if strings.TrimSpace(opts.RootRunID) == "" {
		opts.RootRunID = opts.ParentRunID
	}
	if opts.Plan == nil && len(opts.Children) == 0 {
		finished := clock().UTC()
		report := NestedScheduleReport{
			Version:          1,
			RepoPath:         filepath.ToSlash(opts.RepoPath),
			BaseBranch:       opts.BaseBranch,
			ParentRunID:      opts.ParentRunID,
			Status:           NestedStatusSucceeded,
			StartedAt:        state.FormatTimestamp(started),
			FinishedAt:       state.FormatTimestamp(finished),
			ConcurrencyLimit: opts.ConcurrencyLimit,
			Children:         []ChildRunResult{},
			Summary:          NestedSummary{},
		}
		if err := recordNestedParentDone(ctx, opts, report, finished); err != nil {
			return report, err
		}
		return report, nil
	}

	plan := opts.Plan
	var builtPlan ChildPlan
	var err error
	if plan == nil {
		builtPlan, err = BuildChildPlanFromLegacy(opts, started)
		if err != nil {
			return NestedScheduleReport{}, err
		}
		plan = &builtPlan
	} else {
		if err := ValidateChildPlan(plan); err != nil {
			return NestedScheduleReport{}, err
		}
		opts.PlanID = plan.PlanID
		opts.ParentRunID = plan.ParentRunID
		opts.RootRunID = plan.RootRunID
		opts.ParentDepth = plan.ParentDepth
		opts.MaxDepth = plan.MaxDepth
		opts.ConcurrencyLimit = plan.MaxConcurrency
	}
	if len(plan.Items) > opts.MaxChildren {
		return NestedScheduleReport{}, fmt.Errorf("child run count %d exceeds max children %d", len(plan.Items), opts.MaxChildren)
	}
	replay, err := resolveChildPlanReplay(ctx, opts.Store, plan)
	if err != nil {
		return NestedScheduleReport{}, err
	}
	identityTime := started
	if parsed, err := state.ParseTimestamp(plan.CreatedAt); err == nil {
		identityTime = parsed
	}
	children, err := normalizeChildRunPlans(plan.Items, opts, identityTime)
	if err != nil {
		return NestedScheduleReport{}, err
	}
	plan.Items = children
	delegationCapability := NestedExecutorCapability{
		ExecutorID:             "nested-scheduler",
		RegistrationID:         "builtin:nested-scheduler:v1",
		EnforceablePermissions: []string{string(reporter.PermissionReadOnly), string(reporter.PermissionWrite)},
		NativeBridge:           opts.NativeBridge,
	}
	if _, provider, ok := nestedNativeBridgeIdentity(opts.NativeBridge); ok {
		delegationCapability.Provider = provider
	}
	if err := CheckNestedDelegationCapabilities(plan, delegationCapability); err != nil {
		var permissionErr *PermissionNotEnforceableError
		if errors.As(err, &permissionErr) {
			return NestedPermissionRefusalReport(opts.RepoPath, opts.BaseBranch, *plan, delegationCapability, permissionErr, started), nil
		}
		return NestedScheduleReport{}, err
	}
	executionRequests := make([]ChildExecutionRequest, len(children))
	routeDecisions := make([]ChildRouteDecision, len(children))
	routeBlocked := make([]string, len(children))
	for i, child := range children {
		executionRequests[i], err = BuildChildExecutionRequest(opts.RepoPath, *plan, child)
		if err != nil {
			return NestedScheduleReport{}, err
		}
		if opts.ResolveChildRoute == nil {
			continue
		}
		// Parent cancellation must not start new child routing.
		if err := ctx.Err(); err != nil {
			routeBlocked[i] = fmt.Sprintf("parent cancelled before child route: %v", err)
			continue
		}
		// Explicit pins and prior authority on the contract still go through the
		// resolver so permission and capability checks stay uniform.
		decision, routeErr := opts.ResolveChildRoute(ctx, executionRequests[i])
		if routeErr != nil {
			if decision.RoutingDecisionID != "" || decision.ZeroProviderLaunches {
				routeDecisions[i] = decision
				routeBlocked[i] = routeErr.Error()
				if decision.AdapterID != "" {
					// Keep the refused decision on the contract for audit when a
					// durable decision id exists but launch is blocked.
					if applied, applyErr := ApplyChildRouteDecision(executionRequests[i], decision); applyErr == nil {
						executionRequests[i] = applied
					}
				}
				continue
			}
			return NestedScheduleReport{}, fmt.Errorf("child %q route: %w", child.ChildKey, routeErr)
		}
		if decision.ZeroProviderLaunches || strings.TrimSpace(decision.AdapterID) == "" {
			routeDecisions[i] = decision
			if strings.TrimSpace(decision.Outcome) == "" {
				decision.Outcome = "no_route"
			}
			routeBlocked[i] = firstNonEmptyChild(decision.ChosenReason, "no eligible nested child route")
			continue
		}
		applied, applyErr := ApplyChildRouteDecision(executionRequests[i], decision)
		if applyErr != nil {
			return NestedScheduleReport{}, fmt.Errorf("child %q apply route: %w", child.ChildKey, applyErr)
		}
		executionRequests[i] = applied
		routeDecisions[i] = decision
		// Carry route budget authority onto the child plan metadata used by
		// claim/launch reservation (separate from the immutable execution contract).
		children[i].Metadata = mergeNestedRouteBudgetAuthority(children[i].Metadata, decision, applied)
	}
	if err := persistAcceptedChildPlan(ctx, opts, *plan, executionRequests, started); err != nil {
		return NestedScheduleReport{}, err
	}
	executionAuditRequests := make([]ChildExecutionRequest, len(executionRequests))
	for i := range executionRequests {
		executionAuditRequests[i] = cloneChildExecutionRequest(executionRequests[i])
		if opts.Store == nil {
			continue
		}
		persisted, ok, err := storage.LoadChildExecutionRequest(ctx, opts.Store, executionRequests[i].RunID)
		if err != nil {
			return NestedScheduleReport{}, err
		}
		if ok && persisted.LegacyAmbiguous {
			executionAuditRequests[i].SchemaVersion = persisted.SchemaVersion
			executionAuditRequests[i].ContractFingerprint = persisted.ContractFingerprint
		}
	}
	results := make([]ChildRunResult, len(children))
	dispatchJobs := make([]int, 0, len(children))
	plannedAttempts := 0
	for i, child := range children {
		result := childResultFromExecutionRequest(child, executionAuditRequests[i])
		result = applyChildRouteToResult(result, executionAuditRequests[i], routeDecisions[i])
		if result.ReplayAction == "" {
			result.ReplayAction = ReplayActionNew
			children[i].ReplayAction = ReplayActionNew
		}
		if blocked := strings.TrimSpace(routeBlocked[i]); blocked != "" {
			result.Status = NestedStatusNeedsHuman
			result.Error = blocked
			result.Reason = blocked
			result.NextAction = "inspect the nested child route decision before replaying"
			result.FinishedAt = state.FormatTimestamp(started)
			if err := storage.TransitionChildRunStatus(ctx, opts.Store, opts.ParentRunID, child.RunID, result.Status, result.FinishedAt, "nested child route gate"); err != nil {
				return NestedScheduleReport{}, err
			}
			if err := recordNestedEvent(opts, opts.ParentRunID, child, result, NestedEventChildFinished, started); err != nil {
				return NestedScheduleReport{}, err
			}
			emitNestedChildProgress(ctx, opts, child, result, NestedEventChildFinished, started, true)
			results[i] = withNestedDecision(result)
			continue
		}
		if replayed, ok := replay[child.ChildKey]; ok {
			durableStatus := strings.TrimSpace(replayed.Status)
			if durableStatus == "" {
				durableStatus = NestedStatusQueued
			}
			result.Status = normalizeNestedStatus(durableStatus)
			result.StartedAt = replayed.StartedAt
			result.FinishedAt = replayed.FinishedAt
			result.ReplayAction = replayActionForStatus(result.Status)
			children[i].ReplayAction = result.ReplayAction
			switch result.ReplayAction {
			case ReplayActionReused:
				if nestedFailurePropagates(result.Status) {
					if _, err := storage.PropagateRunTreeTerminal(ctx, opts.Store, storage.RunTreeTerminalRequest{
						RunID:     child.RunID,
						Status:    result.Status,
						UpdatedAt: firstNonEmptyChild(result.FinishedAt, state.FormatTimestamp(started)),
						Reason:    "replayed terminal child propagates to accepted descendants",
						Source:    "nested-scheduler",
					}); err != nil {
						return NestedScheduleReport{}, err
					}
				}
				results[i] = result
				continue
			case ReplayActionBlocked:
				if result.Error == "" {
					result.Error = fmt.Sprintf("child %q is %s in durable state; human action is required before replay", child.ChildKey, result.Status)
				}
				if result.FinishedAt == "" {
					result.FinishedAt = state.FormatTimestamp(started)
				}
				if err := recordNestedEvent(opts, opts.ParentRunID, child, result, NestedEventChildFinished, started); err != nil {
					return NestedScheduleReport{}, err
				}
				emitNestedChildProgress(ctx, opts, child, result, NestedEventChildFinished, started, true)
				results[i] = result
				continue
			default:
				result.Status = NestedStatusQueued
				result.StartedAt = ""
				result.FinishedAt = ""
			}
		}
		if err := recordNestedEvent(opts, opts.ParentRunID, child, result, NestedEventChildQueued, started); err != nil {
			return NestedScheduleReport{}, err
		}
		emitNestedChildProgress(ctx, opts, child, result, NestedEventChildQueued, started, false)
		if blocked, ok, err := evaluateNestedBudget(opts, child, plannedAttempts, started); err != nil {
			result.Status = NestedStatusNeedsHuman
			result.Error = err.Error()
			result.FinishedAt = state.FormatTimestamp(started)
			if err := storage.TransitionChildRunStatus(ctx, opts.Store, opts.ParentRunID, child.RunID, result.Status, result.FinishedAt, "nested budget preflight"); err != nil {
				return NestedScheduleReport{}, err
			}
			if err := recordNestedEvent(opts, opts.ParentRunID, child, result, NestedEventChildFinished, started); err != nil {
				return NestedScheduleReport{}, err
			}
			emitNestedChildProgress(ctx, opts, child, result, NestedEventChildFinished, started, true)
			results[i] = result
			continue
		} else if ok {
			result.Status = NestedStatusNeedsHuman
			result.Error = blocked
			result.FinishedAt = state.FormatTimestamp(started)
			if err := storage.TransitionChildRunStatus(ctx, opts.Store, opts.ParentRunID, child.RunID, result.Status, result.FinishedAt, "nested budget preflight"); err != nil {
				return NestedScheduleReport{}, err
			}
			if err := recordNestedEvent(opts, opts.ParentRunID, child, result, NestedEventChildFinished, started); err != nil {
				return NestedScheduleReport{}, err
			}
			emitNestedChildProgress(ctx, opts, child, result, NestedEventChildFinished, started, true)
			results[i] = result
			continue
		}
		plannedAttempts++
		if blocked, ok, err := evaluateNestedCircuit(opts, child, nil, started); err != nil {
			result.Status = NestedStatusNeedsHuman
			result.Error = err.Error()
			result.FinishedAt = state.FormatTimestamp(started)
			if err := storage.TransitionChildRunStatus(ctx, opts.Store, opts.ParentRunID, child.RunID, result.Status, result.FinishedAt, "nested circuit preflight"); err != nil {
				return NestedScheduleReport{}, err
			}
			if err := recordNestedEvent(opts, opts.ParentRunID, child, result, NestedEventChildFinished, started); err != nil {
				return NestedScheduleReport{}, err
			}
			emitNestedChildProgress(ctx, opts, child, result, NestedEventChildFinished, started, true)
			results[i] = result
			continue
		} else if ok {
			result.Status = NestedStatusNeedsHuman
			result.Error = blocked
			result.FinishedAt = state.FormatTimestamp(started)
			if err := storage.TransitionChildRunStatus(ctx, opts.Store, opts.ParentRunID, child.RunID, result.Status, result.FinishedAt, "nested circuit preflight"); err != nil {
				return NestedScheduleReport{}, err
			}
			if err := recordNestedEvent(opts, opts.ParentRunID, child, result, NestedEventChildFinished, started); err != nil {
				return NestedScheduleReport{}, err
			}
			emitNestedChildProgress(ctx, opts, child, result, NestedEventChildFinished, started, true)
			results[i] = result
			continue
		}
		dispatchJobs = append(dispatchJobs, i)
	}

	var eventMu sync.Mutex
	var completeErr error
	suppressParentDone := false
	setCompleteErr := func(err error) {
		if err == nil {
			return
		}
		eventMu.Lock()
		defer eventMu.Unlock()
		if completeErr == nil {
			completeErr = err
		}
	}
	markSuppressParentDone := func() {
		eventMu.Lock()
		defer eventMu.Unlock()
		suppressParentDone = true
	}
	parentDoneSuppressed := func() bool {
		eventMu.Lock()
		defer eventMu.Unlock()
		return suppressParentDone
	}
	runChild := func(index int) {
		child := children[index]
		executionRequest := executionRequests[index]
		result := childResultFromExecutionRequest(child, executionRequest)
		result.Status = NestedStatusRunning
		claimAt := runtimeClock().UTC()
		executorID := nestedExecutorID(opts.ParentRunID)
		nativeAgent := childRequiresNativeRegistration(child)
		var nativeRegistration storage.AgentRegistration
		var claim storage.ClaimResult
		var err error
		if nativeAgent {
			req, buildErr := nativeRegistrationRequestFromChild(opts, child, claimAt)
			if buildErr != nil {
				result.Status = NestedStatusNeedsHuman
				result.Error = buildErr.Error()
				result.NextAction = "provide accepted native child authority metadata before launch"
				result.FinishedAt = state.FormatTimestamp(clock().UTC())
				if persistErr := storage.TransitionChildRunStatus(ctx, opts.Store, opts.ParentRunID, child.RunID, result.Status, result.FinishedAt, "native child authority gate"); persistErr != nil {
					setCompleteErr(persistErr)
					return
				}
				emitNestedChildProgress(ctx, opts, child, result, "nested.child.blocked", parseOrClock(result.FinishedAt, clock), true)
				results[index] = withNestedDecision(result)
				return
			}
			claim, nativeRegistration, err = storage.ClaimAndRegisterNativeChildWithReservations(ctx, opts.Store, opts.ParentRunID, child.RunID, executorID, claimAt, claimAt.Add(nestedClaimLeaseDuration), req, nestedSchedulerReservationRequest(opts, child, req))
		} else {
			claim, err = storage.ClaimChildRunExecutionWithReservations(ctx, opts.Store, opts.ParentRunID, child.RunID, executorID, claimAt, claimAt.Add(nestedClaimLeaseDuration), nestedSchedulerReservationRequest(opts, child, storage.AgentRegistrationRequest{}))
		}
		if err != nil {
			if storage.IsNestedSchedulerResourceExhausted(err) {
				result.Status = NestedStatusNeedsHuman
				result.Error = err.Error()
				result.NextAction = "wait for nested scheduler reservations to release before replaying"
				result.FinishedAt = state.FormatTimestamp(clock().UTC())
				if persistErr := storage.TransitionChildRunStatus(ctx, opts.Store, opts.ParentRunID, child.RunID, result.Status, result.FinishedAt, "nested scheduler reservation gate"); persistErr != nil {
					setCompleteErr(persistErr)
					return
				}
				results[index] = withNestedDecision(result)
				return
			}
			if errors.Is(err, storage.ErrChildBudgetRequired) || errors.Is(err, storage.ErrDuplicateReplay) {
				result.Status = NestedStatusNeedsHuman
				result.Error = err.Error()
				result.NextAction = "provide accepted route authority and live budget capacity before launch"
				result.FinishedAt = state.FormatTimestamp(clock().UTC())
				if persistErr := storage.TransitionChildRunStatus(ctx, opts.Store, opts.ParentRunID, child.RunID, result.Status, result.FinishedAt, "nested scheduler budget gate"); persistErr != nil {
					setCompleteErr(persistErr)
					return
				}
				results[index] = withNestedDecision(result)
				return
			}
			if nativeAgent {
				result.Status = NestedStatusNeedsHuman
				result.Error = err.Error()
				result.NextAction = "fix native child authority, budget, or ownership before launch"
				result.FinishedAt = state.FormatTimestamp(clock().UTC())
				if persistErr := storage.TransitionChildRunStatus(ctx, opts.Store, opts.ParentRunID, child.RunID, result.Status, result.FinishedAt, "native child atomic registration gate"); persistErr != nil {
					setCompleteErr(persistErr)
					return
				}
				emitNestedChildProgress(ctx, opts, child, result, "nested.child.blocked", parseOrClock(result.FinishedAt, clock), true)
				results[index] = withNestedDecision(result)
				return
			}
			setCompleteErr(err)
			return
		}
		result = applyClaimResult(result, claim)
		switch claim.Outcome {
		case storage.ClaimOutcomeClaimed, storage.ClaimOutcomeStaleClaim:
			if claim.Outcome == storage.ClaimOutcomeStaleClaim {
				result.ReplayAction = ReplayActionRetried
				result.Reason = fmt.Sprintf("stale claim from %s expired at %s; claimed generation %d", claim.PreviousOwner, claim.PreviousLease, claim.ClaimGeneration)
				emitNestedChildProgress(ctx, opts, child, result, "nested.child.recovering", claimAt, false)
			}
		case storage.ClaimOutcomeAlreadyRunning:
			result.Status = NestedStatusRunning
			result.ReplayAction = ReplayActionResumed
			result.Reason = fmt.Sprintf("child execution is already owned by %s until %s", claim.ExecutorID, claim.LeaseExpiresAt)
			result.NextAction = "observe the active durable child owner before replaying"
			emitNestedChildProgress(ctx, opts, child, result, NestedEventChildRunning, claimAt, false)
			results[index] = result
			return
		case storage.ClaimOutcomeTerminalReused:
			result.Status = normalizeNestedStatus(firstNonEmptyChild(claim.RunStatus, claim.EdgeStatus))
			result.ReplayAction = ReplayActionReused
			emitNestedChildProgress(ctx, opts, child, result, NestedEventChildFinished, claimAt, true)
			results[index] = result
			return
		case storage.ClaimOutcomeBlocked:
			result.Status = NestedStatusNeedsHuman
			result.ReplayAction = ReplayActionBlocked
			result.Error = fmt.Sprintf("child %q is %s in durable state; human action is required before replay", child.ChildKey, normalizeNestedStatus(firstNonEmptyChild(claim.RunStatus, claim.EdgeStatus)))
			result.FinishedAt = state.FormatTimestamp(clock().UTC())
			emitNestedChildProgress(ctx, opts, child, result, "nested.child.blocked", parseOrClock(result.FinishedAt, clock), true)
			results[index] = withNestedDecision(result)
			return
		default:
			setCompleteErr(fmt.Errorf("claim child run %s returned unknown outcome %q", child.RunID, claim.Outcome))
			return
		}
		phaseAt := clock().UTC()
		boundRecord, bindErr := storage.BindChildExecutionRequestClaim(ctx, opts.Store, child.RunID, claim.ExecutorID, claim.ClaimGeneration, executionRequest.ContractFingerprint, storage.ClaimPhaseClaimed, state.FormatTimestamp(phaseAt))
		if bindErr != nil {
			result.Status = NestedStatusNeedsHuman
			result.Error = bindErr.Error()
			result.NextAction = "inspect the durable child execution contract before replaying"
			result.FinishedAt = state.FormatTimestamp(clock().UTC())
			if persistErr := storage.RejectClaimedChildExecutionRequest(ctx, opts.Store, opts.ParentRunID, child.RunID, claim.ExecutorID, claim.ClaimGeneration, result.FinishedAt, "child execution contract gate"); persistErr != nil {
				setCompleteErr(persistErr)
				markSuppressParentDone()
				return
			}
			results[index] = withNestedDecision(result)
			return
		}
		if opts.Store != nil {
			executionRequest, err = childExecutionRequestFromRecord(boundRecord)
		} else {
			executionRequest, err = bindChildExecutionRequest(executionRequest, claim.ClaimGeneration, storage.ClaimPhaseClaimed)
		}
		if err != nil {
			result.Status = NestedStatusNeedsHuman
			result.Error = err.Error()
			result.NextAction = "repair the durable child execution contract before replaying"
			result.FinishedAt = state.FormatTimestamp(clock().UTC())
			if persistErr := storage.RejectClaimedChildExecutionRequest(ctx, opts.Store, opts.ParentRunID, child.RunID, claim.ExecutorID, claim.ClaimGeneration, result.FinishedAt, "child execution contract decode gate"); persistErr != nil {
				setCompleteErr(persistErr)
				markSuppressParentDone()
				return
			}
			results[index] = withNestedDecision(result)
			return
		}
		if nativeAgent {
			nativeRegistration, err = storage.ValidateNativeChildLaunch(ctx, opts.Store, child.RunID, claim.ExecutorID, claim.ClaimGeneration)
			if err != nil {
				result.Status = NestedStatusNeedsHuman
				result.Error = err.Error()
				result.NextAction = "register the provider-native child before launch"
				result.FinishedAt = state.FormatTimestamp(clock().UTC())
				if persistErr := storage.TransitionChildRunStatus(ctx, opts.Store, opts.ParentRunID, child.RunID, result.Status, result.FinishedAt, "native child registration gate"); persistErr != nil {
					setCompleteErr(persistErr)
					return
				}
				emitNestedChildProgress(ctx, opts, child, result, "nested.child.blocked", parseOrClock(result.FinishedAt, clock), true)
				results[index] = withNestedDecision(result)
				return
			}
			if nativeRegistration, err = storage.TransitionAgentRegistration(ctx, opts.Store, nativeRegistration.ChildAgentID, storage.AgentActionLaunch, claim.ExecutorID, claim.ClaimGeneration, state.FormatTimestamp(phaseAt)); err != nil {
				result.Status = NestedStatusNeedsHuman
				result.Error = err.Error()
				result.NextAction = "observe native child registration before replaying"
				result.FinishedAt = state.FormatTimestamp(clock().UTC())
				_ = storage.TransitionChildRunStatus(ctx, opts.Store, opts.ParentRunID, child.RunID, result.Status, result.FinishedAt, "native child launch transition failed")
				emitNestedChildProgress(ctx, opts, child, result, "nested.child.blocked", parseOrClock(result.FinishedAt, clock), true)
				results[index] = withNestedDecision(result)
				return
			}
		}
		if err := storage.UpdateChildRunClaimPhase(ctx, opts.Store, opts.ParentRunID, child.RunID, claim.ExecutorID, claim.ClaimGeneration, storage.ClaimPhaseExecuting, state.FormatTimestamp(phaseAt), ""); err != nil {
			if storage.IsStaleChildRunClaim(err) {
				result.Status = NestedStatusNeedsHuman
				result.Error = err.Error()
				result.NextAction = "observe the current durable child owner before replaying"
				results[index] = withNestedDecision(result)
				markSuppressParentDone()
				emitNestedChildProgress(ctx, opts, child, result, "nested.child.blocked", phaseAt, true)
				return
			}
			setCompleteErr(err)
			return
		}
		result.ClaimPhase = storage.ClaimPhaseExecuting
		executionRequest.LifecycleStatus = NestedStatusRunning
		if nativeAgent {
			if nativeRegistration, err = storage.TransitionAgentRegistration(ctx, opts.Store, nativeRegistration.ChildAgentID, storage.AgentActionHeartbeat, claim.ExecutorID, claim.ClaimGeneration, state.FormatTimestamp(phaseAt)); err != nil {
				result.Status = NestedStatusNeedsHuman
				result.Error = err.Error()
				result.NextAction = "observe native child registration before replaying"
				result.FinishedAt = state.FormatTimestamp(clock().UTC())
				_ = storage.TransitionChildRunStatus(ctx, opts.Store, opts.ParentRunID, child.RunID, result.Status, result.FinishedAt, "native child running transition failed")
				emitNestedChildProgress(ctx, opts, child, result, "nested.child.blocked", parseOrClock(result.FinishedAt, clock), true)
				results[index] = withNestedDecision(result)
				return
			}
		}
		child.ProviderKey = claim.ProviderKey
		if result.StartedAt == "" {
			result.StartedAt = state.FormatTimestamp(phaseAt)
		}
		eventMu.Lock()
		err = recordNestedEvent(opts, opts.ParentRunID, child, result, NestedEventChildRunning, parseOrClock(result.StartedAt, clock))
		eventMu.Unlock()
		setCompleteErr(err)
		if err := recordNestedEvent(opts, child.RunID, child, result, NestedEventChildRunning, parseOrClock(result.StartedAt, clock)); err != nil {
			setCompleteErr(err)
		}
		emitNestedChildProgress(ctx, opts, child, result, NestedEventChildRunning, parseOrClock(result.StartedAt, clock), false)

		stopHeartbeat := startNestedClaimHeartbeat(opts.Store, child.RunID, claim.ExecutorID, claim.ClaimGeneration, runtimeClock)
		executor := opts.Execute
		if nativeAgent {
			executor = opts.NativeBridge.Execute
		}
		executed, err := executor(ctx, cloneChildExecutionRequest(executionRequest))
		heartbeatErr := stopHeartbeat()
		if contractErr := validateChildExecutionResult(executionRequest, executed); contractErr != nil {
			executed = ChildRunResult{
				Status:     NestedStatusNeedsHuman,
				Error:      contractErr.Error(),
				Reason:     contractErr.Error(),
				NextAction: "inspect the executor boundary before replaying this child",
			}
			err = nil
		}
		if err == nil && strings.TrimSpace(executed.Status) == "" {
			executed.Status = NestedStatusFailed
		}
		result = mergeChildResult(result, executed)
		if err != nil {
			var policyViolation interface{ ChildExecutionPolicyViolation() }
			if errors.As(err, &policyViolation) {
				outcome := NestedOutcomeReadOnlyPolicyViolation
				var outcomeError interface{ ChildExecutionPolicyOutcome() string }
				if errors.As(err, &outcomeError) {
					if candidate := strings.TrimSpace(outcomeError.ChildExecutionPolicyOutcome()); candidate != "" {
						outcome = candidate
					}
				}
				result.Status = NestedStatusNeedsHuman
				result.Outcome = outcome
				result.Error = err.Error()
				if outcome == NestedOutcomeWriteScopePolicyViolation {
					result.Reason = "the bounded-write child changed or could not conclusively verify an authorized state surface"
					result.NextAction = "inspect the preserved bounded-write enforcement evidence before any replay"
				} else {
					result.Reason = "the read-only child changed or could not conclusively verify a guarded state surface"
					result.NextAction = "inspect the preserved read-only enforcement evidence before any replay"
				}
			} else {
				result.Status = normalizeNestedStatus(state.FailureStatus(err))
				result.Error = err.Error()
			}
		}
		result.Status = normalizeNestedStatus(result.Status)
		if result.FinishedAt == "" {
			result.FinishedAt = state.FormatTimestamp(clock())
		}
		result = applyNestedCircuitOutcome(opts, child, result, parseOrClock(result.FinishedAt, clock))
		results[index] = result

		finishedAt := parseOrClock(result.FinishedAt, clock)
		if heartbeatErr != nil && !storage.IsStaleChildRunClaim(heartbeatErr) {
			setCompleteErr(heartbeatErr)
		}
		completeCtx, cancelComplete := nestedCleanupContext()
		completeClaimErr := storage.CompleteClaimedChildRun(completeCtx, opts.Store, opts.ParentRunID, child.RunID, claim.ExecutorID, claim.ClaimGeneration, result.Status, result.FinishedAt, "child provider finished", result.ProviderReceipt)
		cancelComplete()
		if completeClaimErr != nil {
			markSuppressParentDone()
		}
		if storage.IsStaleChildRunClaim(completeClaimErr) {
			// A stale owner may retain private evidence for diagnosis, but it must
			// never publish a mutation manifest, provider receipt, or attempt as if
			// its generation had completed the durable child run.
			result.MutationManifest = nil
			result.ReadOnlyEnforcement = nil
			result.WorktreePath = ""
			result.ProviderReceipt = ""
			result.AttemptPath = ""
			result.RecoveryContextPath = ""
			result.Report = nil
			result.Outcome = ""
			result.Reason = ""
			result.FinishedAt = ""
			result.Status = NestedStatusNeedsHuman
			result.Error = completeClaimErr.Error()
			result.NextAction = "observe the current durable child owner before publishing terminal state"
			results[index] = withNestedDecision(result)
			emitNestedChildProgress(ctx, opts, child, result, "nested.child.blocked", finishedAt, true)
			return
		}
		setCompleteErr(completeClaimErr)
		if completeClaimErr != nil {
			return
		}
		eventMu.Lock()
		err = recordNestedEvent(opts, opts.ParentRunID, child, result, NestedEventChildFinished, finishedAt)
		eventMu.Unlock()
		setCompleteErr(err)
		if err := recordNestedEvent(opts, child.RunID, child, result, NestedEventChildFinished, finishedAt); err != nil {
			setCompleteErr(err)
		}
		emitNestedChildProgress(ctx, opts, child, result, NestedEventChildFinished, finishedAt, true)
		setCompleteErr(notifyTaskBoundaryRouteReevaluation(ctx, opts, child, result, finishedAt))
	}
	pending := map[int]bool{}
	for _, index := range dispatchJobs {
		pending[index] = true
	}
	emittedWaiting := map[int]bool{}
	for len(pending) > 0 {
		for index := range pending {
			if blocked, ok := blockedByNestedDependency(children[index], children, results); ok {
				delete(pending, index)
				result := childResultFromExecutionRequest(children[index], executionAuditRequests[index])
				result.Status = NestedStatusBlocked
				result.Error = blocked
				finishedAt := clock().UTC()
				result.FinishedAt = state.FormatTimestamp(finishedAt)
				setCompleteErr(storage.TransitionChildRunStatus(ctx, opts.Store, opts.ParentRunID, children[index].RunID, result.Status, result.FinishedAt, "nested dependency blocked"))
				eventMu.Lock()
				err := recordNestedEvent(opts, opts.ParentRunID, children[index], result, NestedEventChildFinished, finishedAt)
				eventMu.Unlock()
				setCompleteErr(err)
				if err := recordNestedEvent(opts, children[index].RunID, children[index], result, NestedEventChildFinished, finishedAt); err != nil {
					setCompleteErr(err)
				}
				emitNestedChildProgress(ctx, opts, children[index], result, NestedEventChildFinished, finishedAt, true)
				results[index] = withNestedDecision(result)
			}
		}
		if len(pending) == 0 {
			break
		}
		ready := readyNestedChildren(children, results, pending)
		if len(ready) == 0 {
			return NestedScheduleReport{}, fmt.Errorf("no ready nested children remain; dependency cycle or missing result escaped validation")
		}
		for index := range pending {
			if emittedWaiting[index] || intSliceContains(ready, index) {
				continue
			}
			waiting := childResultFromExecutionRequest(children[index], executionAuditRequests[index])
			waiting.Status = NestedStatusWaiting
			emitNestedChildProgress(ctx, opts, children[index], waiting, "nested.child.waiting", clock().UTC(), false)
			emittedWaiting[index] = true
		}
		var wg sync.WaitGroup
		sem := make(chan struct{}, opts.ConcurrencyLimit)
		for _, index := range ready {
			delete(pending, index)
			select {
			case <-ctx.Done():
				result := childResultFromExecutionRequest(children[index], executionAuditRequests[index])
				result.Status = normalizeNestedStatus(state.FailureStatus(ctx.Err()))
				if result.Status == "" {
					result.Status = NestedStatusCancelled
				}
				result.Error = ctx.Err().Error()
				finishedAt := clock().UTC()
				result.FinishedAt = state.FormatTimestamp(finishedAt)
				persistCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				setCompleteErr(storage.TransitionChildRunStatus(persistCtx, opts.Store, opts.ParentRunID, children[index].RunID, result.Status, result.FinishedAt, "parent context stopped before child launch"))
				cancel()
				eventMu.Lock()
				err := recordNestedEvent(opts, opts.ParentRunID, children[index], result, NestedEventChildFinished, finishedAt)
				eventMu.Unlock()
				setCompleteErr(err)
				if err := recordNestedEvent(opts, children[index].RunID, children[index], result, NestedEventChildFinished, finishedAt); err != nil {
					setCompleteErr(err)
				}
				emitNestedChildProgress(context.Background(), opts, children[index], result, NestedEventChildFinished, finishedAt, true)
				results[index] = result
				continue
			case sem <- struct{}{}:
			}
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				defer func() { <-sem }()
				runChild(index)
			}(index)
		}
		wg.Wait()
	}

	for _, result := range results {
		if strings.TrimSpace(result.RunID) == "" || strings.TrimSpace(result.Status) == "" || !nestedFailurePropagates(result.Status) {
			continue
		}
		updatedAt := firstNonEmptyChild(result.FinishedAt, state.FormatTimestamp(clock().UTC()))
		propagateCtx := ctx
		var cancelPropagate context.CancelFunc
		if ctx.Err() != nil {
			propagateCtx, cancelPropagate = nestedCleanupContext()
		}
		_, err := storage.PropagateRunTreeTerminal(propagateCtx, opts.Store, storage.RunTreeTerminalRequest{
			RunID:     result.RunID,
			Status:    result.Status,
			UpdatedAt: updatedAt,
			Reason:    "terminal child status propagates to accepted descendants",
			Source:    "nested-scheduler",
		})
		if cancelPropagate != nil {
			cancelPropagate()
		}
		if err != nil {
			return NestedScheduleReport{}, err
		}
	}

	finished := clock().UTC()
	report := NestedScheduleReport{
		Version:          1,
		RepoPath:         filepath.ToSlash(opts.RepoPath),
		BaseBranch:       opts.BaseBranch,
		ParentRunID:      opts.ParentRunID,
		StartedAt:        state.FormatTimestamp(started),
		FinishedAt:       state.FormatTimestamp(finished),
		ConcurrencyLimit: opts.ConcurrencyLimit,
		Children:         results,
	}
	report.Summary = nestedSummary(results)
	report.Status = nestedParentStatus(results)
	for _, child := range results {
		if isNestedPolicyViolationOutcome(child.Outcome) {
			report.Outcome = child.Outcome
			report.Status = NestedStatusNeedsHuman
			break
		}
	}
	if !parentDoneSuppressed() {
		parentCtx := ctx
		var cancelParent context.CancelFunc
		if ctx.Err() != nil {
			parentCtx, cancelParent = nestedCleanupContext()
		}
		if err := recordNestedParentDone(parentCtx, opts, report, finished); err != nil && completeErr == nil {
			completeErr = err
		}
		if cancelParent != nil {
			cancelParent()
		}
	}
	if completeErr != nil {
		return report, completeErr
	}
	return report, nil
}

func notifyTaskBoundaryRouteReevaluation(ctx context.Context, opts NestedScheduleOptions, child ChildRunPlan, result ChildRunResult, finishedAt time.Time) error {
	if opts.TaskBoundaryRouteReevaluation == nil || !nestedStatusTerminal(result.Status) {
		return nil
	}
	if finishedAt.IsZero() {
		finishedAt = parseOrClock(result.FinishedAt, opts.Clock)
	}
	return opts.TaskBoundaryRouteReevaluation(ctx, TaskBoundaryRouteReevaluationEvent{
		ParentRunID: opts.ParentRunID,
		ChildRunID:  child.RunID,
		ChildKey:    child.ChildKey,
		Status:      result.Status,
		FinishedAt:  finishedAt.UTC(),
	})
}

func emitNestedChildProgress(ctx context.Context, opts NestedScheduleOptions, child ChildRunPlan, result ChildRunResult, event string, at time.Time, terminal bool) {
	if opts.Progress == nil {
		return
	}
	if at.IsZero() {
		if opts.Clock != nil {
			at = opts.Clock()
		} else {
			at = opts.Now
		}
	}
	known := knownNestedProgressState(result.Status, event)
	observation := progress.Observation{
		DeliveryRunID: opts.RootRunID,
		RunID:         child.RunID,
		TaskID:        child.ChildKey,
		AttemptID:     result.ProviderKey,
		CorrelationID: "nested-child:" + child.RunID,
		Phase:         strings.TrimSpace(event),
		Status:        strings.TrimSpace(result.Status),
		KnownState:    known,
		Reason:        progress.ReasonStateChange,
		TaskCounts:    nestedProgressCounts(result.Status),
		Provider: progress.ProviderIdentity{
			ProviderID:         strings.TrimSpace(child.Role),
			ProviderConfidence: "unknown",
		},
		Evidence: []progress.EvidenceRef{{
			RecordKind:     "nested-child-run",
			RecordID:       child.RunID,
			Summary:        "nested child state changed via " + strings.TrimSpace(event),
			Classification: "local-diagnostic",
			Confidence:     "exact",
		}},
		OccurredAt: at.UTC(),
		Terminal:   terminal,
	}
	if strings.TrimSpace(observation.Status) == "" {
		observation.Status = progress.Unknown
	}
	if result.Error != "" {
		observation.Blocker = progress.ActionState{State: "blocked", Summary: boundedNestedProgressText(result.Error)}
	}
	if result.NextAction != "" {
		observation.NextAction = progress.ActionState{State: "next", Summary: boundedNestedProgressText(result.NextAction)}
	}
	var err error
	if terminal {
		_, err = opts.Progress.Terminal(ctx, observation)
	} else {
		_, err = opts.Progress.Emit(ctx, observation)
	}
	if err != nil && !errors.Is(err, progress.ErrEmitterClosed) {
		progress.ReportDiagnostic(ctx, opts.Progress, observation, err)
	}
}

func knownNestedProgressState(status, event string) string {
	status = strings.TrimSpace(status)
	event = strings.TrimSpace(event)
	switch {
	case event == "nested.child.recovering":
		return progress.KnownRecoveryInProgress
	case status == NestedStatusWaiting:
		return progress.KnownDeliveryPending
	case status == NestedStatusQueued || status == NestedStatusRunning:
		return progress.KnownDeliveryPending
	case status == NestedStatusCancelled:
		return progress.KnownCancellationInProgress
	case status == NestedStatusNeedsHuman || status == NestedStatusAbandoned || status == NestedStatusBlocked || status == NestedStatusLost:
		return progress.KnownBlocked
	case nestedStatusTerminal(status):
		return progress.KnownTerminal
	default:
		return ""
	}
}

func nestedProgressCounts(status string) progress.TaskCounts {
	counts := progress.TaskCounts{Total: 1}
	switch strings.TrimSpace(status) {
	case NestedStatusQueued, NestedStatusWaiting:
		counts.Ready = 1
	case NestedStatusRunning:
		counts.Running = 1
	case NestedStatusSucceeded, NestedStatusSucceededWithOptionalFailures:
		counts.Succeeded = 1
	case NestedStatusFailed, NestedStatusTimedOut, NestedStatusCancelled, NestedStatusSkipped, NestedStatusAbandoned, NestedStatusLost:
		counts.Failed = 1
	case NestedStatusNeedsHuman, NestedStatusBlocked:
		counts.Blocked = 1
	default:
		counts.Unknown = 1
	}
	return counts
}

func nestedStatusTerminal(status string) bool {
	switch strings.TrimSpace(status) {
	case NestedStatusSucceeded, NestedStatusSucceededWithOptionalFailures, NestedStatusFailed, NestedStatusNeedsHuman, NestedStatusBlocked, NestedStatusLost, NestedStatusSkipped, NestedStatusCancelled, NestedStatusTimedOut, NestedStatusAbandoned:
		return true
	default:
		return false
	}
}

func boundedNestedProgressText(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 180 {
		return value[:180]
	}
	return value
}

func intSliceContains(values []int, want int) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func normalizeChildRunPlans(children []ChildRunPlan, opts NestedScheduleOptions, started time.Time) ([]ChildRunPlan, error) {
	if len(children) == 0 {
		return nil, nil
	}
	if len(children) > opts.MaxChildren {
		return nil, fmt.Errorf("child run count %d exceeds max children %d", len(children), opts.MaxChildren)
	}
	out := make([]ChildRunPlan, 0, len(children))
	seenRunIDs := map[string]bool{}
	for index, child := range children {
		child.ChildKey = strings.TrimSpace(child.ChildKey)
		if child.ChildKey == "" {
			child.ChildKey = strings.TrimSpace(child.ID)
		}
		if child.ChildKey == "" {
			return nil, fmt.Errorf("child[%d].child_key is required", index)
		}
		child.ID = child.ChildKey
		child.Permission = normalizeChildPermission(child.Permission)
		child.Scope = normalizeStructuredScope(child.Scope, child.Issue, child.ScopeIssues)
		child.Issue = firstPositive(child.Issue, firstScopeIssue(child.Scope))
		child.ScopeIssues = append([]int(nil), child.Scope.Issues...)
		child.Depth = opts.ParentDepth + 1
		if child.Depth > opts.MaxDepth {
			return nil, fmt.Errorf("child %q depth %d exceeds max depth %d", child.ChildKey, child.Depth, opts.MaxDepth)
		}
		if child.RunID == "" {
			child.RunID = state.RunIDForChild(child.ChildKey, index, started)
		}
		if !state.IsRunID(child.RunID) {
			return nil, fmt.Errorf("child %q run id %q is invalid", child.ChildKey, child.RunID)
		}
		if child.RunID == opts.ParentRunID {
			return nil, fmt.Errorf("child %q run id %q cannot reuse parent run id", child.ChildKey, child.RunID)
		}
		if child.RunID == opts.RootRunID {
			return nil, fmt.Errorf("child %q run id %q cannot reuse root run id", child.ChildKey, child.RunID)
		}
		if seenRunIDs[child.RunID] {
			return nil, fmt.Errorf("duplicate child run id %q", child.RunID)
		}
		seenRunIDs[child.RunID] = true
		out = append(out, child)
	}
	return out, nil
}

type childReplayState struct {
	Status     string
	StartedAt  string
	FinishedAt string
}

func resolveChildPlanReplay(ctx context.Context, store storage.Store, plan *ChildPlan) (map[string]childReplayState, error) {
	out := map[string]childReplayState{}
	if store == nil || plan == nil {
		return out, nil
	}
	replay, ok, err := storage.LoadChildPlanReplayRecord(ctx, store, plan.PlanID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return out, nil
	}
	var stored ChildPlan
	if err := json.Unmarshal([]byte(replay.Plan.PlanJSON), &stored); err != nil {
		return nil, fmt.Errorf("child plan %q replay rejected: stored plan_json is invalid: %w", plan.PlanID, err)
	}
	if err := ValidateChildPlan(&stored); err != nil {
		return nil, fmt.Errorf("child plan %q replay rejected: stored plan_json is invalid: %w", plan.PlanID, err)
	}
	if left, right := childPlanFingerprint(*plan), childPlanFingerprint(stored); left != right {
		diff := firstChildPlanContractDiff(*plan, stored)
		if diff == "" {
			diff = "plan fingerprint"
		}
		return nil, fmt.Errorf("child plan %q mutation rejected: immutable plan fingerprint changed at %s; use a new plan_id for revisions", plan.PlanID, diff)
	}
	byKey := map[string]storage.ChildPlanReplayChild{}
	for _, child := range replay.Children {
		key := strings.TrimSpace(child.ChildKey)
		if key == "" {
			continue
		}
		byKey[key] = child
	}
	for i := range plan.Items {
		item := &plan.Items[i]
		child, ok := byKey[item.ChildKey]
		if !ok {
			return nil, fmt.Errorf("child plan %q replay rejected: durable edge for child_key %q is missing", plan.PlanID, item.ChildKey)
		}
		if strings.TrimSpace(item.RunID) != "" && strings.TrimSpace(item.RunID) != child.ChildRunID {
			return nil, fmt.Errorf("child plan %q mutation rejected: child %q run_id changed from %q to %q; use a new plan_id for revisions", plan.PlanID, item.ChildKey, child.ChildRunID, item.RunID)
		}
		var scope ChildScope
		if err := json.Unmarshal([]byte(firstNonEmptyChild(child.ScopeJSON, "{}")), &scope); err != nil {
			return nil, fmt.Errorf("child plan %q replay rejected: child %q stored scope_json is invalid: %w", plan.PlanID, item.ChildKey, err)
		}
		var aggregation ChildAggregation
		if err := json.Unmarshal([]byte(firstNonEmptyChild(child.AggregationJSON, "{}")), &aggregation); err != nil {
			return nil, fmt.Errorf("child plan %q replay rejected: child %q stored aggregation_json is invalid: %w", plan.PlanID, item.ChildKey, err)
		}
		item.RunID = child.ChildRunID
		item.Ordinal = child.Ordinal
		item.Depth = child.Depth
		item.Scope = scope
		item.Issue = firstPositive(item.Issue, firstScopeIssue(scope))
		item.ScopeIssues = append([]int(nil), scope.Issues...)
		item.Permission = child.Permission
		item.Aggregation = aggregation
		item.Required = aggregation.Required
		item.Optional = !aggregation.Required
		out[item.ChildKey] = childReplayState{
			Status:     firstNonEmptyChild(child.RunStatus, child.EdgeStatus),
			StartedAt:  child.StartedAt,
			FinishedAt: child.FinishedAt,
		}
	}
	plan.ParentRunID = replay.Plan.ParentRunID
	plan.RootRunID = replay.Plan.RootRunID
	plan.MaxDepth = replay.Plan.MaxDepth
	plan.MaxConcurrency = replay.Plan.MaxConcurrency
	return out, nil
}

func childPlanFingerprint(plan ChildPlan) string {
	contract := canonicalChildPlanContract(plan)
	data, err := json.Marshal(contract)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", sum[:])
}

func canonicalChildPlanContract(plan ChildPlan) ChildPlan {
	clone := plan
	clone.Items = cloneChildPlans(plan.Items)
	for i := range clone.Items {
		clone.Items[i].RunID = ""
		clone.Items[i].ReplayAction = ""
	}
	return clone
}

func firstChildPlanContractDiff(left, right ChildPlan) string {
	var leftAny any
	var rightAny any
	leftData, _ := json.Marshal(canonicalChildPlanContract(left))
	rightData, _ := json.Marshal(canonicalChildPlanContract(right))
	_ = json.Unmarshal(leftData, &leftAny)
	_ = json.Unmarshal(rightData, &rightAny)
	return firstJSONDiff(leftAny, rightAny, "")
}

func firstJSONDiff(left, right any, path string) string {
	if reflect.DeepEqual(left, right) {
		return ""
	}
	leftMap, leftMapOK := left.(map[string]any)
	rightMap, rightMapOK := right.(map[string]any)
	if leftMapOK && rightMapOK {
		keys := map[string]bool{}
		for key := range leftMap {
			keys[key] = true
		}
		for key := range rightMap {
			keys[key] = true
		}
		ordered := make([]string, 0, len(keys))
		for key := range keys {
			ordered = append(ordered, key)
		}
		sort.Strings(ordered)
		for _, key := range ordered {
			next := key
			if path != "" {
				next = path + "." + key
			}
			if diff := firstJSONDiff(leftMap[key], rightMap[key], next); diff != "" {
				return diff
			}
		}
	}
	leftSlice, leftSliceOK := left.([]any)
	rightSlice, rightSliceOK := right.([]any)
	if leftSliceOK && rightSliceOK {
		limit := len(leftSlice)
		if len(rightSlice) < limit {
			limit = len(rightSlice)
		}
		for i := 0; i < limit; i++ {
			next := fmt.Sprintf("%s[%d]", path, i)
			if diff := firstJSONDiff(leftSlice[i], rightSlice[i], next); diff != "" {
				return diff
			}
		}
		return fmt.Sprintf("%s.length", path)
	}
	return path
}

func persistAcceptedChildPlan(ctx context.Context, opts NestedScheduleOptions, plan ChildPlan, requests []ChildExecutionRequest, started time.Time) error {
	if opts.Store == nil {
		return nil
	}
	if len(requests) != len(plan.Items) {
		return fmt.Errorf("persist accepted child plan: child/request count mismatch")
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return fmt.Errorf("marshal accepted child plan: %w", err)
	}
	createdAt := state.FormatTimestamp(started)
	parent := storage.RunNode{
		RunID:     plan.ParentRunID,
		RootRunID: plan.RootRunID,
		Depth:     plan.ParentDepth,
		Origin:    "nested_parent",
		Status:    state.StatusRunning,
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	children := make([]storage.RunNode, 0, len(plan.Items))
	edges := make([]storage.RunEdgeRecord, 0, len(plan.Items))
	requestRecords := make([]storage.ChildExecutionRequestRecord, 0, len(plan.Items))
	for i, item := range plan.Items {
		scopeJSON, err := json.Marshal(item.Scope)
		if err != nil {
			return fmt.Errorf("marshal child %q scope: %w", item.ChildKey, err)
		}
		aggregationJSON, err := json.Marshal(item.Aggregation)
		if err != nil {
			return fmt.Errorf("marshal child %q aggregation: %w", item.ChildKey, err)
		}
		children = append(children, storage.RunNode{
			RunID:       item.RunID,
			ParentRunID: plan.ParentRunID,
			RootRunID:   plan.RootRunID,
			Depth:       item.Depth,
			Origin:      "sub_agent",
			Status:      NestedStatusQueued,
			CreatedAt:   createdAt,
			UpdatedAt:   createdAt,
		})
		edges = append(edges, storage.RunEdgeRecord{
			ParentRunID:     plan.ParentRunID,
			ChildRunID:      item.RunID,
			RootRunID:       plan.RootRunID,
			PlanID:          plan.PlanID,
			ChildKey:        item.ChildKey,
			Depth:           item.Depth,
			Ordinal:         item.Ordinal,
			ScopeJSON:       string(scopeJSON),
			Permission:      item.Permission,
			AggregationJSON: string(aggregationJSON),
			Status:          NestedStatusQueued,
			CreatedAt:       createdAt,
			UpdatedAt:       createdAt,
		})
		requestJSON, err := json.Marshal(requests[i])
		if err != nil {
			return fmt.Errorf("marshal child %q execution request: %w", item.ChildKey, err)
		}
		requestRecords = append(requestRecords, storage.ChildExecutionRequestRecord{
			ChildRunID:          requests[i].RunID,
			ParentRunID:         requests[i].ParentRunID,
			PlanID:              requests[i].PlanID,
			ChildKey:            requests[i].ChildKey,
			SchemaVersion:       requests[i].SchemaVersion,
			RequestJSON:         string(requestJSON),
			ContractFingerprint: requests[i].ContractFingerprint,
			RepositoryIdentity:  requests[i].RepositoryIdentity,
			CheckoutIdentity:    requests[i].CheckoutIdentity,
			Permission:          requests[i].Permission,
			ScopeJSON:           string(scopeJSON),
			ClaimGeneration:     requests[i].ClaimGeneration,
			LifecycleStatus:     requests[i].LifecycleStatus,
			CreatedAt:           createdAt,
			UpdatedAt:           createdAt,
		})
	}
	return storage.PersistChildPlanGraphWithExecutionRequests(ctx, opts.Store, parent, children, storage.ChildPlanRecord{
		PlanID:         plan.PlanID,
		ParentRunID:    plan.ParentRunID,
		RootRunID:      plan.RootRunID,
		SchemaVersion:  plan.SchemaVersion,
		MaxDepth:       plan.MaxDepth,
		MaxConcurrency: plan.MaxConcurrency,
		PlanJSON:       string(planJSON),
		CreatedAt:      createdAt,
	}, edges, requestRecords)
}

func readyNestedChildren(children []ChildRunPlan, results []ChildRunResult, pending map[int]bool) []int {
	indexByKey := map[string]int{}
	for i, child := range children {
		indexByKey[child.ChildKey] = i
	}
	ready := make([]int, 0, len(pending))
	for index := range pending {
		child := children[index]
		depsReady := true
		for _, dep := range child.DependsOn {
			depIndex, ok := indexByKey[dep]
			if !ok || strings.TrimSpace(results[depIndex].Status) == "" {
				depsReady = false
				break
			}
		}
		if depsReady {
			ready = append(ready, index)
		}
	}
	sort.Ints(ready)
	return ready
}

func blockedByNestedDependency(child ChildRunPlan, children []ChildRunPlan, results []ChildRunResult) (string, bool) {
	indexByKey := map[string]int{}
	for i, candidate := range children {
		indexByKey[candidate.ChildKey] = i
	}
	for _, dep := range child.DependsOn {
		depIndex, ok := indexByKey[dep]
		if !ok {
			continue
		}
		rawStatus := strings.TrimSpace(results[depIndex].Status)
		if rawStatus == "" {
			continue
		}
		status := normalizeNestedStatus(rawStatus)
		if status == NestedStatusQueued || status == NestedStatusRunning || status == NestedStatusWaiting {
			continue
		}
		if status == NestedStatusSucceeded || status == NestedStatusSucceededWithOptionalFailures {
			continue
		}
		return fmt.Sprintf("dependency %q ended with status %s", dep, status), true
	}
	return "", false
}

func nestedFailurePropagates(status string) bool {
	switch normalizeNestedStatus(status) {
	case NestedStatusFailed, NestedStatusCancelled, NestedStatusTimedOut, NestedStatusAbandoned, NestedStatusNeedsHuman, NestedStatusBlocked, NestedStatusLost:
		return true
	default:
		return false
	}
}

func childResultFromPlan(child ChildRunPlan) ChildRunResult {
	return ChildRunResult{
		ID:           child.ID,
		ChildKey:     child.ChildKey,
		Title:        child.Title,
		Role:         child.Role,
		RunID:        child.RunID,
		Issue:        child.Issue,
		Scope:        cloneChildScope(child.Scope),
		Permission:   child.Permission,
		DependsOn:    append([]string(nil), child.DependsOn...),
		Aggregation:  child.Aggregation,
		Required:     child.Required,
		Optional:     child.Optional,
		Ordinal:      child.Ordinal,
		Depth:        child.Depth,
		Status:       NestedStatusQueued,
		ReplayAction: child.ReplayAction,
	}
}

func childResultFromExecutionRequest(child ChildRunPlan, request ChildExecutionRequest) ChildRunResult {
	result := childResultFromPlan(child)
	result.ContractSchema = request.SchemaVersion
	result.ContractFingerprint = request.ContractFingerprint
	result.RoutingDecisionID = strings.TrimSpace(request.ProviderDecision.RoutingDecisionID)
	result.RouteAdapterID = firstNonEmptyChild(request.ProviderDecision.AdapterID, request.Work.Provider)
	result.RouteModel = firstNonEmptyChild(request.Work.Model, request.ProviderDecision.ModelCapabilityID)
	return result
}

func mergeChildResult(base, result ChildRunResult) ChildRunResult {
	if strings.TrimSpace(result.ID) != "" {
		base.ID = strings.TrimSpace(result.ID)
	}
	if strings.TrimSpace(result.ChildKey) != "" {
		base.ChildKey = strings.TrimSpace(result.ChildKey)
	}
	if strings.TrimSpace(result.Title) != "" {
		base.Title = strings.TrimSpace(result.Title)
	}
	if strings.TrimSpace(result.Role) != "" {
		base.Role = strings.TrimSpace(result.Role)
	}
	if strings.TrimSpace(result.RunID) != "" {
		base.RunID = strings.TrimSpace(result.RunID)
	}
	if result.Issue > 0 {
		base.Issue = result.Issue
	}
	if !childScopeEmpty(result.Scope) {
		base.Scope = cloneChildScope(result.Scope)
	}
	if strings.TrimSpace(result.Permission) != "" {
		base.Permission = normalizeChildPermission(result.Permission)
	}
	if len(result.DependsOn) > 0 {
		base.DependsOn = append([]string(nil), result.DependsOn...)
	}
	if strings.TrimSpace(result.Aggregation.Mode) != "" {
		base.Aggregation = result.Aggregation
	}
	if result.Required || result.Optional {
		base.Required = result.Required
		base.Optional = result.Optional
	}
	if result.Ordinal > 0 {
		base.Ordinal = result.Ordinal
	}
	if result.Depth > 0 {
		base.Depth = result.Depth
	}
	if strings.TrimSpace(result.Status) != "" {
		base.Status = strings.TrimSpace(result.Status)
	}
	if strings.TrimSpace(result.Outcome) != "" {
		base.Outcome = strings.TrimSpace(result.Outcome)
	}
	if strings.TrimSpace(result.ReplayAction) != "" {
		base.ReplayAction = strings.TrimSpace(result.ReplayAction)
	}
	if strings.TrimSpace(result.ClaimOutcome) != "" {
		base.ClaimOutcome = strings.TrimSpace(result.ClaimOutcome)
	}
	if strings.TrimSpace(result.ClaimOwner) != "" {
		base.ClaimOwner = strings.TrimSpace(result.ClaimOwner)
	}
	if result.ClaimGeneration > 0 {
		base.ClaimGeneration = result.ClaimGeneration
	}
	if strings.TrimSpace(result.LeaseExpiresAt) != "" {
		base.LeaseExpiresAt = strings.TrimSpace(result.LeaseExpiresAt)
	}
	if strings.TrimSpace(result.ClaimPhase) != "" {
		base.ClaimPhase = strings.TrimSpace(result.ClaimPhase)
	}
	if strings.TrimSpace(result.ProviderKey) != "" {
		base.ProviderKey = strings.TrimSpace(result.ProviderKey)
	}
	if strings.TrimSpace(result.ProviderReceipt) != "" {
		base.ProviderReceipt = strings.TrimSpace(result.ProviderReceipt)
	}
	base.Error = strings.TrimSpace(result.Error)
	base.Reason = strings.TrimSpace(result.Reason)
	base.NextAction = strings.TrimSpace(result.NextAction)
	base.AttemptPath = strings.TrimSpace(result.AttemptPath)
	base.RecoveryContextPath = strings.TrimSpace(result.RecoveryContextPath)
	base.Report = result.Report
	if result.ReadOnlyEnforcement != nil {
		audit := *result.ReadOnlyEnforcement
		audit.Violations = append([]state.ReadOnlyEnforcementViolation(nil), result.ReadOnlyEnforcement.Violations...)
		base.ReadOnlyEnforcement = &audit
	}
	if result.MutationManifest != nil {
		audit := *result.MutationManifest
		audit.Changes = append([]state.MutationManifestChange(nil), result.MutationManifest.Changes...)
		audit.Violations = append([]state.MutationManifestViolation(nil), result.MutationManifest.Violations...)
		base.MutationManifest = &audit
	}
	if strings.TrimSpace(result.WorktreePath) != "" {
		base.WorktreePath = strings.TrimSpace(result.WorktreePath)
	}
	if strings.TrimSpace(result.StartedAt) != "" {
		base.StartedAt = strings.TrimSpace(result.StartedAt)
	}
	if strings.TrimSpace(result.FinishedAt) != "" {
		base.FinishedAt = strings.TrimSpace(result.FinishedAt)
	}
	return base
}

func isNestedPolicyViolationOutcome(outcome string) bool {
	switch strings.TrimSpace(outcome) {
	case NestedOutcomeReadOnlyPolicyViolation, NestedOutcomeWriteScopePolicyViolation:
		return true
	default:
		return false
	}
}

func nestedExecutorID(parentRunID string) string {
	seq := atomic.AddUint64(&nestedExecutorSequence, 1)
	return fmt.Sprintf("nested-scheduler:%s:%d:%d", strings.TrimSpace(parentRunID), os.Getpid(), seq)
}

func applyClaimResult(result ChildRunResult, claim storage.ClaimResult) ChildRunResult {
	result.ClaimOutcome = claim.Outcome
	result.ClaimOwner = claim.ExecutorID
	result.ClaimGeneration = claim.ClaimGeneration
	result.LeaseExpiresAt = claim.LeaseExpiresAt
	result.ClaimPhase = claim.ClaimPhase
	result.ProviderKey = claim.ProviderKey
	if strings.TrimSpace(claim.ClaimedAt) != "" {
		result.StartedAt = claim.ClaimedAt
	}
	return result
}

func nestedSchedulerReservationRequest(opts NestedScheduleOptions, child ChildRunPlan, native storage.AgentRegistrationRequest) storage.SchedulerResourceReservationRequest {
	limit := opts.ConcurrencyLimit
	if limit <= 0 {
		limit = lcdefaults.NestedSchedulerMaxConcurrency
	}
	authority := schedulerAuthorityFromChild(child)
	if strings.TrimSpace(native.AdapterID) != "" {
		authority.ProjectID = native.ProjectID
		authority.DeliveryRunID = native.DeliveryRunID
		authority.TaskID = native.TaskID
		authority.SubAgentID = storage.ChildAgentIDForRegistration(native)
		authority.AdapterID = native.AdapterID
		authority.ProviderInstallationID = native.ProviderInstallationID
		authority.AccountProfileID = native.AccountProfileID
		authority.ModelCapabilityID = native.ModelCapabilityID
		authority.RoutingDecisionID = native.RoutingDecisionID
		authority.PlanFingerprint = native.PlanFingerprint
		authority.PolicyFingerprint = native.PolicyFingerprint
		authority.AuthorizationFingerprint = native.AuthorizationFingerprint
	}
	return storage.SchedulerResourceReservationRequest{
		RootRunID:                opts.RootRunID,
		ProviderKey:              nestedSchedulerProviderRoute(child, authority),
		RootMaxConcurrency:       limit,
		ParentMaxConcurrency:     limit,
		ProviderMaxConcurrency:   limit,
		ProjectID:                authority.ProjectID,
		DeliveryRunID:            authority.DeliveryRunID,
		TaskID:                   authority.TaskID,
		SubAgentID:               authority.SubAgentID,
		AdapterID:                authority.AdapterID,
		ProviderInstallationID:   authority.ProviderInstallationID,
		AccountProfileID:         authority.AccountProfileID,
		ModelCapabilityID:        authority.ModelCapabilityID,
		RoutingDecisionID:        authority.RoutingDecisionID,
		RoutingFingerprint:       authority.RoutingFingerprint,
		PlanFingerprint:          authority.PlanFingerprint,
		PolicyFingerprint:        authority.PolicyFingerprint,
		AuthorizationFingerprint: authority.AuthorizationFingerprint,
		BudgetRequestedValue:     authority.BudgetRequestedValue,
		BudgetQuantityKind:       authority.BudgetQuantityKind,
		BudgetUnit:               authority.BudgetUnit,
		BudgetValueScale:         authority.BudgetValueScale,
		BudgetWindowKind:         authority.BudgetWindowKind,
		AttachBudgetBinding:      strings.TrimSpace(native.AdapterID) != "",
		RequireBudgetAuthority:   !opts.AllowUnbudgetedLocalTest,
	}
}

func schedulerAuthorityFromChild(child ChildRunPlan) schedulerAuthorityMetadata {
	if len(child.Metadata) == 0 {
		return schedulerAuthorityMetadata{}
	}
	var authority schedulerAuthorityMetadata
	if err := json.Unmarshal(child.Metadata, &authority); err != nil {
		return schedulerAuthorityMetadata{}
	}
	if strings.TrimSpace(authority.SubAgentID) == "" {
		authority.SubAgentID = strings.TrimSpace(child.RunID)
	}
	return authority
}

// mergeNestedRouteBudgetAuthority copies production route budget fields into
// child plan metadata so claim/launch can reserve against a durable policy.
func mergeNestedRouteBudgetAuthority(existing []byte, decision ChildRouteDecision, request ChildExecutionRequest) []byte {
	authority := schedulerAuthorityMetadata{}
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &authority)
	}
	set := func(dst *string, value string) {
		if v := strings.TrimSpace(value); v != "" {
			*dst = v
		}
	}
	set(&authority.ProjectID, decision.ProjectID)
	set(&authority.DeliveryRunID, decision.DeliveryRunID)
	set(&authority.TaskID, decision.TaskID)
	set(&authority.AdapterID, decision.AdapterID)
	set(&authority.ProviderInstallationID, decision.ProviderInstallationID)
	set(&authority.AccountProfileID, decision.AccountProfileID)
	set(&authority.ModelCapabilityID, decision.ModelCapabilityID)
	set(&authority.RoutingDecisionID, decision.RoutingDecisionID)
	set(&authority.RoutingFingerprint, decision.RoutingFingerprint)
	set(&authority.PlanFingerprint, decision.PlanFingerprint)
	set(&authority.PolicyFingerprint, decision.PolicyFingerprint)
	set(&authority.AuthorizationFingerprint, decision.AuthorizationFingerprint)
	if request.RunID != "" {
		authority.SubAgentID = strings.TrimSpace(request.RunID)
	}
	if decision.BudgetRequestedValue > 0 {
		authority.BudgetRequestedValue = decision.BudgetRequestedValue
	} else if authority.BudgetRequestedValue <= 0 && strings.TrimSpace(decision.RoutingDecisionID) != "" {
		authority.BudgetRequestedValue = 1
	}
	if strings.TrimSpace(authority.BudgetQuantityKind) == "" {
		authority.BudgetQuantityKind = "local-policy"
	}
	if strings.TrimSpace(authority.BudgetUnit) == "" {
		authority.BudgetUnit = "local-policy-unit"
	}
	if strings.TrimSpace(authority.BudgetWindowKind) == "" {
		authority.BudgetWindowKind = "unbounded"
	}
	payload, err := json.Marshal(authority)
	if err != nil {
		return existing
	}
	return payload
}

func nestedSchedulerProviderRoute(child ChildRunPlan, authority schedulerAuthorityMetadata) string {
	parts := []string{
		strings.TrimSpace(authority.RoutingDecisionID),
		strings.TrimSpace(authority.AdapterID),
		strings.TrimSpace(authority.ProviderInstallationID),
		strings.TrimSpace(authority.AccountProfileID),
		strings.TrimSpace(authority.ModelCapabilityID),
	}
	if strings.TrimSpace(strings.Join(parts, "")) == "" {
		return strings.TrimSpace(child.ProviderKey)
	}
	for i := range parts {
		if parts[i] == "" {
			parts[i] = "_"
		}
	}
	return strings.Join(parts, ":")
}

func startNestedClaimHeartbeat(store storage.Store, childRunID, executorID string, generation int64, clock func() time.Time) func() error {
	if store == nil || strings.TrimSpace(childRunID) == "" || strings.TrimSpace(executorID) == "" || generation <= 0 {
		return func() error { return nil }
	}
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	interval := nestedClaimRenewEvery
	if interval <= 0 {
		interval = nestedClaimLeaseDuration / 3
	}
	if interval <= 0 {
		interval = time.Second
	}
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var lastErr error
		for {
			select {
			case <-stop:
				done <- lastErr
				return
			case <-ticker.C:
				now := clock().UTC()
				renewCtx, cancel := nestedCleanupContext()
				err := storage.RenewChildRunClaim(renewCtx, store, childRunID, executorID, generation, now, now.Add(nestedClaimLeaseDuration))
				cancel()
				if err != nil {
					done <- err
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() error {
		once.Do(func() { close(stop) })
		return <-done
	}
}

func nestedCleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), nestedClaimCleanupTimeout)
}

func evaluateNestedBudget(opts NestedScheduleOptions, child ChildRunPlan, plannedAttempts int, now time.Time) (string, bool, error) {
	if !opts.Budget.Enabled() {
		return "", false, nil
	}
	decision := guardrails.EvaluateBudget(guardrails.BudgetOptions{
		RepoPath:         opts.RepoPath,
		RunID:            opts.ParentRunID,
		BaseBranch:       opts.BaseBranch,
		Issue:            childGuardrailIssue(child),
		ScopeIssues:      childGuardrailScope(child),
		Budget:           opts.Budget,
		PlannedAttempts:  plannedAttempts,
		ProposedAttempts: 1,
		Now:              now,
	})
	if _, err := guardrails.RecordDecision(opts.RepoPath, decision); err != nil {
		return "", false, fmt.Errorf("needs-human: guardrails.budget ledger write failed: %v", err)
	}
	if !decision.Allowed {
		return decision.Message, true, nil
	}
	return "", false, nil
}

func evaluateNestedCircuit(opts NestedScheduleOptions, child ChildRunPlan, outcome *guardrails.CircuitOutcome, now time.Time) (string, bool, error) {
	if !opts.CircuitBreaker.Enabled() {
		return "", false, nil
	}
	decision := guardrails.EvaluateCircuitBreaker(guardrails.CircuitOptions{
		RepoPath:       opts.RepoPath,
		RunID:          opts.ParentRunID,
		BaseBranch:     opts.BaseBranch,
		Issue:          childGuardrailIssue(child),
		ScopeIssues:    childGuardrailScope(child),
		CircuitBreaker: opts.CircuitBreaker,
		Outcome:        outcome,
		Now:            now,
	})
	if _, err := guardrails.RecordDecision(opts.RepoPath, decision); err != nil {
		return "", false, fmt.Errorf("needs-human: guardrails.circuit_breaker ledger write failed: %v", err)
	}
	if !decision.Allowed {
		return decision.Message, true, nil
	}
	return "", false, nil
}

func applyNestedCircuitOutcome(opts NestedScheduleOptions, child ChildRunPlan, result ChildRunResult, now time.Time) ChildRunResult {
	if !opts.CircuitBreaker.Enabled() || result.Status == NestedStatusNeedsHuman {
		return result
	}
	blockedMessage, blocked, err := evaluateNestedCircuit(opts, child, &guardrails.CircuitOutcome{
		Kind:             guardrails.CircuitOutcomeWave,
		MaterialProgress: nestedResultMaterialProgress(result),
		Detail:           result.Status,
	}, now)
	if err != nil {
		result.Status = NestedStatusNeedsHuman
		result.Error = err.Error()
		return withNestedDecision(result)
	}
	if blocked {
		result.Status = NestedStatusNeedsHuman
		result.Error = blockedMessage
	}
	return withNestedDecision(result)
}

func withNestedDecision(result ChildRunResult) ChildRunResult {
	if !nestedActionableStatus(result.Status) && strings.TrimSpace(result.Reason) == "" && strings.TrimSpace(result.NextAction) == "" && strings.TrimSpace(result.Error) == "" {
		return result
	}
	receipt := reporter.NormalizeDecision(reporter.DecisionInput{
		Status:             result.Status,
		ExplicitReason:     result.Reason,
		ConcreteError:      result.Error,
		ExplicitNextAction: result.NextAction,
	})
	result.Reason = receipt.Reason
	result.NextAction = receipt.NextAction
	return result
}

func nestedActionableStatus(status string) bool {
	switch normalizeNestedStatus(status) {
	case "", NestedStatusSucceeded, NestedStatusSucceededWithOptionalFailures, NestedStatusQueued, NestedStatusRunning, NestedStatusWaiting:
		return false
	default:
		return true
	}
}

func nestedResultMaterialProgress(result ChildRunResult) bool {
	if result.Status == NestedStatusSucceeded {
		return true
	}
	return strings.TrimSpace(result.AttemptPath) != "" || strings.TrimSpace(result.ReportHeader()) != ""
}

func (r ChildRunResult) ReportHeader() string {
	if r.Report == nil {
		return ""
	}
	return r.Report.Header()
}

func childGuardrailIssue(child ChildRunPlan) int {
	if child.Issue > 0 {
		return child.Issue
	}
	for _, issue := range child.Scope.Issues {
		if issue > 0 {
			return issue
		}
	}
	return 1
}

func childGuardrailScope(child ChildRunPlan) []int {
	if len(child.Scope.Issues) > 0 {
		return append([]int(nil), child.Scope.Issues...)
	}
	if len(child.ScopeIssues) > 0 {
		return append([]int(nil), child.ScopeIssues...)
	}
	if child.Issue > 0 {
		return []int{child.Issue}
	}
	return []int{childGuardrailIssue(child)}
}

func normalizeNestedStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case NestedStatusSucceeded, "success", "completed", "complete", "done":
		return NestedStatusSucceeded
	case NestedStatusSucceededWithOptionalFailures, "succeeded-with-optional-failures", "succeeded with optional failures":
		return NestedStatusSucceededWithOptionalFailures
	case NestedStatusNeedsHuman, "needs_human", "needs human", "hung":
		return NestedStatusNeedsHuman
	case NestedStatusBlocked:
		return NestedStatusBlocked
	case NestedStatusLost:
		return NestedStatusLost
	case NestedStatusSkipped:
		return NestedStatusSkipped
	case NestedStatusCancelled, "canceled":
		return NestedStatusCancelled
	case NestedStatusTimedOut, "timeout", "timed-out":
		return NestedStatusTimedOut
	case NestedStatusAbandoned:
		return NestedStatusAbandoned
	case NestedStatusQueued:
		return NestedStatusQueued
	case NestedStatusRunning, "interrupted":
		return NestedStatusRunning
	case NestedStatusWaiting:
		return NestedStatusWaiting
	case "", NestedStatusFailed, "failure", "error":
		return NestedStatusFailed
	default:
		return strings.ToLower(strings.TrimSpace(status))
	}
}

type nativeChildAuthorityMetadata struct {
	NativeSubagent           bool                         `json:"native_subagent"`
	ProviderNativeSubagent   bool                         `json:"provider_native_subagent"`
	ExecutionKind            string                       `json:"execution_kind"`
	ProjectID                string                       `json:"project_id"`
	DeliveryRunID            string                       `json:"delivery_run_id"`
	TaskID                   string                       `json:"task_id"`
	AttemptID                string                       `json:"attempt_id"`
	AdapterID                string                       `json:"adapter_id"`
	ProviderInstallationID   string                       `json:"provider_installation_id"`
	AccountProfileID         string                       `json:"account_profile_id"`
	ModelCapabilityID        string                       `json:"model_capability_id"`
	RoutingDecisionID        string                       `json:"routing_decision_id"`
	ProviderSessionRef       string                       `json:"provider_session_ref"`
	PlanFingerprint          string                       `json:"plan_fingerprint"`
	PolicyFingerprint        string                       `json:"policy_fingerprint"`
	AuthorizationFingerprint string                       `json:"authorization_fingerprint"`
	SideEffectClass          string                       `json:"side_effect_class"`
	CancellationChannel      string                       `json:"cancellation_channel"`
	ExpectedOutputs          json.RawMessage              `json:"expected_outputs"`
	ParentScope              *storage.AgentScopeGrant     `json:"parent_scope"`
	Scope                    *storage.AgentScopeGrant     `json:"scope"`
	BudgetBindings           []storage.AgentBudgetBinding `json:"budget_bindings"`
	OwnershipLocks           []storage.AgentOwnershipLock `json:"ownership_locks"`
}

type schedulerAuthorityMetadata struct {
	ProjectID                string `json:"project_id"`
	DeliveryRunID            string `json:"delivery_run_id"`
	TaskID                   string `json:"task_id"`
	SubAgentID               string `json:"sub_agent_id"`
	AdapterID                string `json:"adapter_id"`
	ProviderInstallationID   string `json:"provider_installation_id"`
	AccountProfileID         string `json:"account_profile_id"`
	ModelCapabilityID        string `json:"model_capability_id"`
	RoutingDecisionID        string `json:"routing_decision_id"`
	RoutingFingerprint       string `json:"routing_fingerprint"`
	PlanFingerprint          string `json:"plan_fingerprint"`
	PolicyFingerprint        string `json:"policy_fingerprint"`
	AuthorizationFingerprint string `json:"authorization_fingerprint"`
	BudgetRequestedValue     int64  `json:"budget_requested_value"`
	BudgetQuantityKind       string `json:"budget_quantity_kind"`
	BudgetUnit               string `json:"budget_unit"`
	BudgetValueScale         int    `json:"budget_value_scale"`
	BudgetWindowKind         string `json:"budget_window_kind"`
}

func nativeRegistrationRequestFromChild(opts NestedScheduleOptions, child ChildRunPlan, at time.Time) (storage.AgentRegistrationRequest, error) {
	var metadata nativeChildAuthorityMetadata
	if err := json.Unmarshal(child.Metadata, &metadata); err != nil {
		return storage.AgentRegistrationRequest{}, fmt.Errorf("native child metadata is invalid: %w", err)
	}
	expectedOutputs := strings.TrimSpace(string(metadata.ExpectedOutputs))
	if expectedOutputs == "" {
		return storage.AgentRegistrationRequest{}, fmt.Errorf("native child metadata is missing expected_outputs")
	}
	if !json.Valid([]byte(expectedOutputs)) {
		return storage.AgentRegistrationRequest{}, fmt.Errorf("native child metadata expected_outputs is invalid")
	}
	if metadata.Scope == nil {
		return storage.AgentRegistrationRequest{}, fmt.Errorf("native child metadata is missing scope")
	}
	if metadata.ParentScope == nil {
		return storage.AgentRegistrationRequest{}, fmt.Errorf("native child metadata is missing parent_scope")
	}
	scope := *metadata.Scope
	sideEffectClass := strings.TrimSpace(metadata.SideEffectClass)
	if sideEffectClass == "" {
		return storage.AgentRegistrationRequest{}, fmt.Errorf("native child metadata is missing side_effect_class")
	}
	if len(scope.SideEffectScope) == 0 {
		return storage.AgentRegistrationRequest{}, fmt.Errorf("native child metadata scope is missing side_effect_scope")
	}
	if strings.TrimSpace(metadata.CancellationChannel) == "" {
		return storage.AgentRegistrationRequest{}, fmt.Errorf("native child metadata is missing cancellation_channel")
	}
	if len(metadata.BudgetBindings) == 0 {
		return storage.AgentRegistrationRequest{}, fmt.Errorf("native child metadata is missing budget_bindings")
	}
	locks := append([]storage.AgentOwnershipLock{}, metadata.OwnershipLocks...)
	if len(locks) == 0 && (child.Permission == storage.PermissionWrite || child.Permission == storage.PermissionOrchestrate || len(scope.WriteScope) > 0) {
		return storage.AgentRegistrationRequest{}, fmt.Errorf("native child metadata is missing ownership_locks")
	}
	return storage.AgentRegistrationRequest{
		ProjectID:                metadata.ProjectID,
		DeliveryRunID:            metadata.DeliveryRunID,
		RootRunID:                opts.RootRunID,
		ParentRunID:              opts.ParentRunID,
		RunID:                    child.RunID,
		Depth:                    child.Depth,
		TaskID:                   metadata.TaskID,
		AttemptID:                metadata.AttemptID,
		PlanID:                   opts.PlanID,
		ChildKey:                 child.ChildKey,
		AdapterID:                metadata.AdapterID,
		ProviderInstallationID:   metadata.ProviderInstallationID,
		AccountProfileID:         metadata.AccountProfileID,
		ModelCapabilityID:        metadata.ModelCapabilityID,
		RoutingDecisionID:        metadata.RoutingDecisionID,
		ProviderSessionRef:       metadata.ProviderSessionRef,
		Scope:                    scope,
		ParentScope:              metadata.ParentScope,
		Permission:               child.Permission,
		SideEffectClass:          sideEffectClass,
		BudgetBindings:           metadata.BudgetBindings,
		OwnershipLocks:           locks,
		CancellationChannel:      metadata.CancellationChannel,
		ExpectedOutputsJSON:      expectedOutputs,
		PlanFingerprint:          metadata.PlanFingerprint,
		PolicyFingerprint:        metadata.PolicyFingerprint,
		AuthorizationFingerprint: metadata.AuthorizationFingerprint,
		CreatedAt:                state.FormatTimestamp(at.UTC()),
	}, nil
}

func childRequiresNativeRegistration(child ChildRunPlan) bool {
	if len(child.Metadata) == 0 {
		return false
	}
	var metadata struct {
		NativeSubagent         bool   `json:"native_subagent"`
		ProviderNativeSubagent bool   `json:"provider_native_subagent"`
		ExecutionKind          string `json:"execution_kind"`
	}
	if err := json.Unmarshal(child.Metadata, &metadata); err != nil {
		return false
	}
	if metadata.NativeSubagent || metadata.ProviderNativeSubagent {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(metadata.ExecutionKind)) {
	case "native-subagent", "provider-native-subagent":
		return true
	default:
		return false
	}
}

func replayActionForStatus(status string) string {
	switch normalizeNestedStatus(status) {
	case NestedStatusSucceeded, NestedStatusSucceededWithOptionalFailures:
		return ReplayActionReused
	case NestedStatusQueued, NestedStatusRunning, NestedStatusWaiting:
		return ReplayActionResumed
	case NestedStatusFailed, NestedStatusCancelled, NestedStatusTimedOut:
		return ReplayActionReused
	case NestedStatusNeedsHuman, NestedStatusBlocked, NestedStatusLost, NestedStatusAbandoned, NestedStatusSkipped:
		return ReplayActionBlocked
	default:
		return ReplayActionResumed
	}
}

func nestedParentStatus(results []ChildRunResult) string {
	needsHuman := false
	optionalFailures := false
	for _, result := range results {
		if result.Aggregation.Mode == ChildAggregationIgnore {
			continue
		}
		if result.Aggregation.Mode == ChildAggregationGate {
			switch result.Status {
			case NestedStatusSucceeded:
			case NestedStatusRunning, NestedStatusQueued, NestedStatusWaiting, "pending":
				return NestedStatusRunning
			default:
				needsHuman = true
			}
			continue
		}
		if !result.Required {
			if nestedOptionalFailureStatus(result.Status) {
				optionalFailures = true
			}
			continue
		}
		switch result.Status {
		case NestedStatusRunning, NestedStatusQueued, NestedStatusWaiting, "pending":
			return NestedStatusRunning
		case NestedStatusFailed, NestedStatusCancelled, NestedStatusTimedOut, NestedStatusAbandoned, NestedStatusSkipped, NestedStatusNeedsHuman, NestedStatusBlocked, NestedStatusLost, "hung", "idle":
			needsHuman = true
		}
	}
	if needsHuman {
		return NestedStatusNeedsHuman
	}
	if optionalFailures {
		return NestedStatusSucceededWithOptionalFailures
	}
	return NestedStatusSucceeded
}

func nestedOptionalFailureStatus(status string) bool {
	switch normalizeNestedStatus(status) {
	case NestedStatusFailed, NestedStatusCancelled, NestedStatusTimedOut, NestedStatusAbandoned, NestedStatusNeedsHuman, NestedStatusBlocked, NestedStatusLost:
		return true
	default:
		return false
	}
}

func nestedSummary(results []ChildRunResult) NestedSummary {
	var summary NestedSummary
	for _, result := range results {
		if result.Required {
			summary.RequiredCount++
		}
		if result.Optional {
			summary.OptionalCount++
		}
		switch result.Status {
		case NestedStatusSucceeded:
			summary.SucceededCount++
		case NestedStatusFailed:
			summary.FailedCount++
		case NestedStatusNeedsHuman:
			summary.NeedsHumanCount++
		case NestedStatusBlocked, NestedStatusLost:
			summary.NeedsHumanCount++
		case NestedStatusSkipped:
			summary.SkippedCount++
		case NestedStatusCancelled:
			summary.CancelledCount++
		}
	}
	return summary
}

func recordNestedEvent(opts NestedScheduleOptions, runID string, child ChildRunPlan, result ChildRunResult, eventName string, at time.Time) error {
	details, err := json.Marshal(nestedChildEventDetails{
		ParentRunID: opts.ParentRunID,
		Child:       child,
		Result:      result,
	})
	if err != nil {
		return fmt.Errorf("marshal nested child event details: %w", err)
	}
	eventStatus := nestedEventStatus(eventName, result.Status)
	return opts.RecordEvent(opts.RepoPath, runID, state.Event{
		Timestamp: state.FormatTimestamp(at),
		RunID:     runID,
		JobID:     "nested-scheduler",
		Issue:     child.Issue,
		Phase:     "nested-scheduler",
		Status:    eventStatus,
		LogBytes:  0,
		Event:     eventName,
		Outcome:   eventStatus,
		Details:   json.RawMessage(details),
	})
}

func nestedEventStatus(eventName, resultStatus string) string {
	switch eventName {
	case NestedEventChildQueued:
		return state.StatusQueued
	case NestedEventChildRunning:
		return state.StatusRunning
	default:
		return resultStatus
	}
}

func recordNestedParentDone(ctx context.Context, opts NestedScheduleOptions, report NestedScheduleReport, at time.Time) error {
	if err := storage.TransitionParentRunStatus(ctx, opts.Store, opts.ParentRunID, report.Status, state.FormatTimestamp(at), "nested parent finished"); err != nil {
		return err
	}
	details, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("marshal nested parent event details: %w", err)
	}
	return opts.RecordEvent(opts.RepoPath, opts.ParentRunID, state.Event{
		Timestamp: state.FormatTimestamp(at),
		RunID:     opts.ParentRunID,
		JobID:     "nested-scheduler",
		Phase:     "nested-scheduler",
		Status:    report.Status,
		LogBytes:  0,
		Event:     NestedEventParentDone,
		Outcome:   report.Status,
		Details:   json.RawMessage(details),
	})
}

type nestedChildEventDetails struct {
	ParentRunID string         `json:"parent_run_id"`
	Child       ChildRunPlan   `json:"child"`
	Result      ChildRunResult `json:"result"`
}

func childScopeEmpty(scope ChildScope) bool {
	return strings.TrimSpace(scope.Repo) == "" &&
		len(scope.Paths) == 0 &&
		len(scope.Issues) == 0 &&
		len(scope.PullRequests) == 0 &&
		len(scope.Commands) == 0 &&
		len(scope.Data) == 0
}

func parseOrClock(value string, clock func() time.Time) time.Time {
	if parsed, err := state.ParseTimestamp(value); err == nil {
		return parsed
	}
	return clock().UTC()
}
