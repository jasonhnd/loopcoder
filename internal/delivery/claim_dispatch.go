package delivery

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/progress"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

const (
	OutcomeClaimedDispatch = "claimed_for_dispatch"
	OutcomeNoReadyTask     = "no_ready_task"
)

// ClaimDispatchOptions configures the single-task claim operation.
type ClaimDispatchOptions struct {
	ProjectID                        string
	DeliveryRunID                    string
	ExpectedAuthorizationFingerprint string
	Actor                            Actor
	Host                             Host
	IdempotencyKey                   string
	ExecutorID                       string
	Now                              time.Time
	HostEnforcement                  HostEnforcement
	Progress                         progress.Recorder
}

// ClaimDispatchResult is the bounded receipt for zero-or-one task claim.
type ClaimDispatchResult struct {
	ProjectID                string             `json:"project_id"`
	DeliveryRunID            string             `json:"delivery_run_id"`
	AuthorizationFingerprint string             `json:"authorization_fingerprint"`
	RunState                 string             `json:"run_state"`
	ApprovalStatus           string             `json:"approval_status"`
	Outcome                  string             `json:"outcome"`
	Claimed                  bool               `json:"claimed"`
	TaskID                   string             `json:"task_id,omitempty"`
	TaskKey                  string             `json:"task_key,omitempty"`
	TaskTitle                string             `json:"task_title,omitempty"`
	IssueNumber              int                `json:"issue_number,omitempty"`
	AttemptID                string             `json:"attempt_id,omitempty"`
	ClaimGeneration          int64              `json:"claim_generation,omitempty"`
	ProviderIdempotencyKey   string             `json:"provider_idempotency_key,omitempty"`
	NextAction               string             `json:"next_action"`
	Invocation               InvocationEvidence `json:"invocation"`
	AuthorizedInvocation     InvocationEvidence `json:"authorized_invocation,omitempty"`
	Replayed                 bool               `json:"replayed,omitempty"`
}

// ClaimOneReadyTask selects at most one ready task with deterministic ordering,
// atomically claims it, and returns a handoff receipt. It never launches a
// provider; the CLI/worker pipeline performs route decide + dispatch after claim.
func ClaimOneReadyTask(ctx context.Context, store storage.Store, opts ClaimDispatchOptions) (ClaimDispatchResult, error) {
	if store == nil {
		return ClaimDispatchResult{}, typed(ErrInvalidRecordCode, "store is required")
	}
	if opts.Now.IsZero() {
		return ClaimDispatchResult{}, typed(ErrInvalidRecordCode, "now is required")
	}
	opts.ProjectID = strings.TrimSpace(opts.ProjectID)
	opts.DeliveryRunID = strings.TrimSpace(opts.DeliveryRunID)
	if opts.ProjectID == "" || opts.DeliveryRunID == "" {
		return ClaimDispatchResult{}, typed(ErrInvalidRecordCode, "project_id and delivery_run_id are required")
	}
	if strings.TrimSpace(opts.ExecutorID) == "" {
		opts.ExecutorID = "loopcoder-delivery-claim-dispatch"
	}
	invocation, err := enforceHostInvocation(OperationClaimDispatch, opts.HostEnforcement)
	if err != nil {
		emitHostUnavailableProgress(ctx, opts.Progress, opts.ProjectID, opts.DeliveryRunID, OperationClaimDispatch, err, opts.Now)
		return ClaimDispatchResult{ProjectID: opts.ProjectID, DeliveryRunID: opts.DeliveryRunID, Outcome: OutcomeUnsupported, Invocation: invocation}, err
	}
	if ctx.Err() != nil {
		return ClaimDispatchResult{ProjectID: opts.ProjectID, DeliveryRunID: opts.DeliveryRunID, Outcome: OutcomeInterrupted, Invocation: invocation}, typed(ErrInvocationInterruptedCode, "%s interrupted before mutation", OperationClaimDispatch)
	}

	// Load run authority snapshot for receipt fields and optional stale guard.
	var runState, approvalStatus, authFP string
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT state, approval_status, COALESCE(authorization_fingerprint, '')
			FROM delivery_runs WHERE project_id = ? AND delivery_run_id = ?`,
			opts.ProjectID, opts.DeliveryRunID).Scan(&runState, &approvalStatus, &authFP)
	}); err != nil {
		return ClaimDispatchResult{}, fmt.Errorf("load delivery run: %w", err)
	}
	if expected := strings.TrimSpace(opts.ExpectedAuthorizationFingerprint); expected != "" && expected != authFP {
		return ClaimDispatchResult{
			ProjectID:     opts.ProjectID,
			DeliveryRunID: opts.DeliveryRunID,
			Outcome:       OutcomeStale,
			Invocation:    invocation,
			NextAction:    "re-plan; authorization fingerprint is stale",
		}, ErrStaleApproval
	}
	if runState != RunQueued && runState != RunRunning && runState != RunApproved {
		return ClaimDispatchResult{
			ProjectID:     opts.ProjectID,
			DeliveryRunID: opts.DeliveryRunID,
			RunState:      runState,
			Outcome:       OutcomeUnsupported,
			Invocation:    invocation,
			NextAction:    "delivery run is not in a claimable state; continue or approve first",
		}, typed(ErrInvalidTransitionCode, "delivery run %s is %s", opts.DeliveryRunID, runState)
	}

	// Load ready tasks and pick one deterministically (task_key, then task_id).
	tasks, err := listReadyTasks(ctx, store, opts.ProjectID, opts.DeliveryRunID)
	if err != nil {
		return ClaimDispatchResult{}, err
	}
	if len(tasks) == 0 {
		return ClaimDispatchResult{
			ProjectID:                opts.ProjectID,
			DeliveryRunID:            opts.DeliveryRunID,
			AuthorizationFingerprint: authFP,
			RunState:                 runState,
			ApprovalStatus:           approvalStatus,
			Outcome:                  OutcomeNoReadyTask,
			Claimed:                  false,
			NextAction:               "wait for a ready approved task or inspect blocked/approval dependencies",
			Invocation:               invocation,
		}, nil
	}
	selected := tasks[0]
	attempt, err := ClaimTask(ctx, store, opts.ProjectID, opts.DeliveryRunID, selected.TaskID, opts.ExecutorID, opts.Actor, opts.Host, PersistOptions{
		Now:            opts.Now,
		IdempotencyKey: firstNonEmpty(opts.IdempotencyKey, "claim-dispatch:"+opts.DeliveryRunID+":"+selected.TaskID),
		Progress:       opts.Progress,
	})
	if err != nil {
		return ClaimDispatchResult{
			ProjectID:                opts.ProjectID,
			DeliveryRunID:            opts.DeliveryRunID,
			AuthorizationFingerprint: authFP,
			RunState:                 runState,
			ApprovalStatus:           approvalStatus,
			TaskID:                   selected.TaskID,
			TaskKey:                  selected.TaskKey,
			Outcome:                  OutcomeUnsupported,
			Invocation:               invocation,
			NextAction:               "retry claim after resolving claim conflict: " + err.Error(),
		}, err
	}

	issueNumber := issueNumberFromTask(selected)
	return ClaimDispatchResult{
		ProjectID:                opts.ProjectID,
		DeliveryRunID:            opts.DeliveryRunID,
		AuthorizationFingerprint: authFP,
		RunState:                 runState,
		ApprovalStatus:           approvalStatus,
		Outcome:                  OutcomeClaimedDispatch,
		Claimed:                  true,
		TaskID:                   selected.TaskID,
		TaskKey:                  selected.TaskKey,
		TaskTitle:                selected.Title,
		IssueNumber:              issueNumber,
		AttemptID:                attempt.AttemptID,
		ClaimGeneration:          attempt.ClaimGeneration,
		ProviderIdempotencyKey:   attempt.ProviderIdempotencyKey,
		NextAction:               "route and dispatch the claimed task through ordinary worker path",
		Invocation:               invocation,
	}, nil
}

func listReadyTasks(ctx context.Context, store storage.Store, projectID, deliveryRunID string) ([]Task, error) {
	var tasks []Task
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, `SELECT task_id, task_key, title, state, requirements_json, scope_json, permission, side_effect_class
			FROM delivery_tasks
			WHERE project_id = ? AND delivery_run_id = ? AND state = ?
			ORDER BY task_key ASC, task_id ASC`, projectID, deliveryRunID, TaskReady)
		if err != nil {
			return fmt.Errorf("list ready tasks: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var task Task
			if err := rows.Scan(&task.TaskID, &task.TaskKey, &task.Title, &task.State, &task.RequirementsJSON, &task.ScopeJSON, &task.Permission, &task.SideEffectClass); err != nil {
				return fmt.Errorf("scan ready task: %w", err)
			}
			task.ProjectID = projectID
			task.DeliveryRunID = deliveryRunID
			tasks = append(tasks, task)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(tasks, func(i, j int) bool {
		if tasks[i].TaskKey != tasks[j].TaskKey {
			return tasks[i].TaskKey < tasks[j].TaskKey
		}
		return tasks[i].TaskID < tasks[j].TaskID
	})
	return tasks, nil
}

func issueNumberFromTask(task Task) int {
	// Prefer task_key shapes like "issue-1010" or "worker-issue-42".
	key := strings.ToLower(strings.TrimSpace(task.TaskKey))
	for _, prefix := range []string{"worker-issue-", "issue-"} {
		if strings.HasPrefix(key, prefix) {
			n, err := strconv.Atoi(strings.TrimPrefix(key, prefix))
			if err == nil && n > 0 {
				return n
			}
		}
	}
	// Fallback: parse trailing digits from title "#N".
	title := strings.TrimSpace(task.Title)
	if idx := strings.LastIndex(title, "#"); idx >= 0 {
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimRight(title[idx+1:], " :.-")))
		if err == nil && n > 0 {
			return n
		}
	}
	return 0
}
