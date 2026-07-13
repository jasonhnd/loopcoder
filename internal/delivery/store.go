package delivery

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/progress"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

type cycleDetector func(context.Context, storage.Tx, string, string, string) (bool, error)

type PersistOptions struct {
	IdempotencyKey string
	Now            time.Time
	Progress       *progress.Emitter

	cycleDetector cycleDetector
}

func PersistDeliveryRun(ctx context.Context, store storage.Store, run DeliveryRun, opts PersistOptions) (DeliveryRun, error) {
	if store == nil {
		return DeliveryRun{}, typed(ErrInvalidRecordCode, "store is required")
	}
	now := optionNow(opts)
	run = normalizeDeliveryRun(run, now)
	if err := validateDeliveryRun(run); err != nil {
		return DeliveryRun{}, err
	}
	var out DeliveryRun
	err := withWrite(ctx, store, func(tx storage.Tx) error {
		request := map[string]any{"operation": "persist_delivery_run", "record": run}
		replayed, err := replay(ctx, tx, opts.IdempotencyKey, run.ProjectID, run.DeliveryRunID, "persist_delivery_run", request, &out)
		if err != nil || replayed {
			return err
		}
		createdBy, err := marshalJSON("delivery run created_by", run.CreatedBy)
		if err != nil {
			return err
		}
		updatedBy, err := marshalJSON("delivery run updated_by", run.UpdatedBy)
		if err != nil {
			return err
		}
		host, err := marshalJSON("delivery run host", run.Host)
		if err != nil {
			return err
		}
		reportIDs, err := marshalJSON("delivery run report_ids", run.ReportIDs)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO delivery_runs(
				delivery_run_id, run_id, schema_version, record_version, project_id, root_run_id, parent_run_id, state, intent_summary,
				input_fingerprint, policy_fingerprint, plan_fingerprint, authorization_fingerprint, policy_version, max_side_effect_class,
				approval_status, override_status, created_at, updated_at, planned_at, approved_at, started_at, ended_at,
				created_by_json, updated_by_json, host_json, terminal_error_code, error_message, report_ids_json, legacy_id_source
			) VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(delivery_run_id) DO UPDATE SET
				record_version = delivery_runs.record_version + 1,
				state = excluded.state,
				intent_summary = excluded.intent_summary,
				input_fingerprint = excluded.input_fingerprint,
				policy_fingerprint = excluded.policy_fingerprint,
				plan_fingerprint = excluded.plan_fingerprint,
				authorization_fingerprint = excluded.authorization_fingerprint,
				policy_version = excluded.policy_version,
				max_side_effect_class = excluded.max_side_effect_class,
				approval_status = excluded.approval_status,
				override_status = excluded.override_status,
				updated_at = excluded.updated_at,
				planned_at = COALESCE(delivery_runs.planned_at, excluded.planned_at),
				approved_at = COALESCE(delivery_runs.approved_at, excluded.approved_at),
				started_at = COALESCE(delivery_runs.started_at, excluded.started_at),
				ended_at = COALESCE(delivery_runs.ended_at, excluded.ended_at),
				updated_by_json = excluded.updated_by_json,
				host_json = excluded.host_json,
				terminal_error_code = excluded.terminal_error_code,
				error_message = excluded.error_message,
				report_ids_json = excluded.report_ids_json`,
			run.DeliveryRunID, run.RunID, run.SchemaVersion, run.RecordVersion, run.ProjectID, run.RootRunID, run.ParentRunID, run.State, run.IntentSummary,
			run.InputFingerprint, run.PolicyFingerprint, run.PlanFingerprint, run.AuthorizationFingerprint, run.PolicyVersion, run.MaxSideEffectClass,
			run.ApprovalStatus, run.OverrideStatus, run.CreatedAt, run.UpdatedAt, run.PlannedAt, run.ApprovedAt, run.StartedAt, run.EndedAt,
			createdBy, updatedBy, host, run.TerminalErrorCode, run.ErrorMessage, reportIDs, run.LegacyIDSource); err != nil {
			return fmt.Errorf("persist delivery run: %w", err)
		}
		out = run
		return remember(ctx, tx, opts.IdempotencyKey, run.ProjectID, run.DeliveryRunID, "persist_delivery_run", request, out, now)
	})
	if err == nil {
		emitDeliveryRunProgress(ctx, opts.Progress, out, "persist_delivery_run", optionNow(opts))
	}
	return out, err
}

func PersistPlanFingerprint(ctx context.Context, store storage.Store, fp PlanFingerprint, opts PersistOptions) (PlanFingerprint, error) {
	if store == nil {
		return PlanFingerprint{}, typed(ErrInvalidRecordCode, "store is required")
	}
	now := optionNow(opts)
	fp = normalizePlanFingerprint(fp, now)
	if err := validatePlanFingerprint(fp); err != nil {
		return PlanFingerprint{}, err
	}
	var out PlanFingerprint
	err := withWrite(ctx, store, func(tx storage.Tx) error {
		if err := ensureRunProject(ctx, tx, fp.ProjectID, fp.DeliveryRunID); err != nil {
			return err
		}
		request := map[string]any{"operation": "persist_plan_fingerprint", "record": fp}
		replayed, err := replay(ctx, tx, opts.IdempotencyKey, fp.ProjectID, fp.DeliveryRunID, "persist_plan_fingerprint", request, &out)
		if err != nil || replayed {
			return err
		}
		createdBy, err := marshalJSON("plan fingerprint created_by", fp.CreatedBy)
		if err != nil {
			return err
		}
		host, err := marshalJSON("plan fingerprint host", fp.Host)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO delivery_plan_fingerprints(
				fingerprint_id, schema_version, record_version, project_id, delivery_run_id, input_fingerprint, policy_fingerprint,
				plan_fingerprint, authorization_fingerprint, canonicalization_version, algorithm, canonical_inputs_json, created_at, created_by_json, host_json
			) VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(fingerprint_id) DO NOTHING`,
			fp.FingerprintID, fp.SchemaVersion, fp.ProjectID, fp.DeliveryRunID, fp.InputFingerprint, fp.PolicyFingerprint,
			fp.PlanFingerprint, fp.AuthorizationFingerprint, fp.CanonicalizationVersion, fp.Algorithm, emptyJSON(fp.CanonicalInputsJSON),
			fp.CreatedAt, createdBy, host); err != nil {
			return fmt.Errorf("persist plan fingerprint: %w", err)
		}
		out = fp
		return remember(ctx, tx, opts.IdempotencyKey, fp.ProjectID, fp.DeliveryRunID, "persist_plan_fingerprint", request, out, now)
	})
	return out, err
}

func PersistTask(ctx context.Context, store storage.Store, task Task, opts PersistOptions) (Task, error) {
	if store == nil {
		return Task{}, typed(ErrInvalidRecordCode, "store is required")
	}
	now := optionNow(opts)
	task = normalizeTask(task, now)
	if err := validateTask(task); err != nil {
		return Task{}, err
	}
	var out Task
	err := withWrite(ctx, store, func(tx storage.Tx) error {
		if err := ensureRunProject(ctx, tx, task.ProjectID, task.DeliveryRunID); err != nil {
			return err
		}
		request := map[string]any{"operation": "persist_task", "record": task}
		replayed, err := replay(ctx, tx, opts.IdempotencyKey, task.ProjectID, task.DeliveryRunID, "persist_task", request, &out)
		if err != nil || replayed {
			return err
		}
		if existing, ok, err := taskByKey(ctx, tx, task.ProjectID, task.DeliveryRunID, task.TaskKey); err != nil {
			return err
		} else if ok && existing != task.TaskID {
			return typed(ErrDuplicateRecordCode, "task_key %q already belongs to %s", task.TaskKey, existing)
		}
		createdBy, err := marshalJSON("task created_by", task.CreatedBy)
		if err != nil {
			return err
		}
		updatedBy, err := marshalJSON("task updated_by", task.UpdatedBy)
		if err != nil {
			return err
		}
		host, err := marshalJSON("task host", task.Host)
		if err != nil {
			return err
		}
		dependsOn, err := marshalJSON("task depends_on", task.DependsOn)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO delivery_tasks(
				task_id, schema_version, record_version, project_id, delivery_run_id, task_key, state, title, requirements_json, scope_json,
				permission, side_effect_class, policy_version, plan_fingerprint, authorization_fingerprint, attempt_count, active_attempt_id,
				depends_on_json, created_at, updated_at, ready_at, started_at, ended_at, created_by_json, updated_by_json, host_json,
				terminal_error_code, error_message
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, ?)
			ON CONFLICT(task_id) DO UPDATE SET
				record_version = delivery_tasks.record_version + 1,
				state = excluded.state,
				title = excluded.title,
				requirements_json = excluded.requirements_json,
				scope_json = excluded.scope_json,
				permission = excluded.permission,
				side_effect_class = excluded.side_effect_class,
				policy_version = excluded.policy_version,
				plan_fingerprint = excluded.plan_fingerprint,
				authorization_fingerprint = excluded.authorization_fingerprint,
				attempt_count = excluded.attempt_count,
				active_attempt_id = excluded.active_attempt_id,
				depends_on_json = excluded.depends_on_json,
				updated_at = excluded.updated_at,
				ready_at = COALESCE(delivery_tasks.ready_at, excluded.ready_at),
				started_at = COALESCE(delivery_tasks.started_at, excluded.started_at),
				ended_at = COALESCE(delivery_tasks.ended_at, excluded.ended_at),
				updated_by_json = excluded.updated_by_json,
				host_json = excluded.host_json,
				terminal_error_code = excluded.terminal_error_code,
				error_message = excluded.error_message`,
			task.TaskID, task.SchemaVersion, task.RecordVersion, task.ProjectID, task.DeliveryRunID, task.TaskKey, task.State, task.Title, emptyJSON(task.RequirementsJSON), emptyJSON(task.ScopeJSON),
			task.Permission, task.SideEffectClass, task.PolicyVersion, task.PlanFingerprint, task.AuthorizationFingerprint, task.AttemptCount, task.ActiveAttemptID,
			dependsOn, task.CreatedAt, task.UpdatedAt, task.ReadyAt, task.StartedAt, task.EndedAt, createdBy, updatedBy, host,
			task.TerminalErrorCode, task.ErrorMessage); err != nil {
			return fmt.Errorf("persist task: %w", err)
		}
		out = task
		return remember(ctx, tx, opts.IdempotencyKey, task.ProjectID, task.DeliveryRunID, "persist_task", request, out, now)
	})
	if err == nil {
		emitTaskProgress(ctx, opts.Progress, out, "persist_task", optionNow(opts))
	}
	return out, err
}

func AddDependencyEdge(ctx context.Context, store storage.Store, edge DependencyEdge, opts PersistOptions) (DependencyEdge, error) {
	if store == nil {
		return DependencyEdge{}, typed(ErrInvalidRecordCode, "store is required")
	}
	now := optionNow(opts)
	edge = normalizeEdge(edge, now)
	if err := validateEdge(edge); err != nil {
		return DependencyEdge{}, err
	}
	var out DependencyEdge
	err := withWrite(ctx, store, func(tx storage.Tx) error {
		if err := ensureRunProject(ctx, tx, edge.ProjectID, edge.DeliveryRunID); err != nil {
			return err
		}
		if err := ensureTaskProject(ctx, tx, edge.ProjectID, edge.DeliveryRunID, edge.FromTaskID); err != nil {
			return err
		}
		if err := ensureTaskProject(ctx, tx, edge.ProjectID, edge.DeliveryRunID, edge.ToTaskID); err != nil {
			return err
		}
		request := map[string]any{"operation": "add_dependency_edge", "record": edge}
		replayed, err := replay(ctx, tx, opts.IdempotencyKey, edge.ProjectID, edge.DeliveryRunID, "add_dependency_edge", request, &out)
		if err != nil || replayed {
			return err
		}
		if err := ensureDependencyEdgeAbsent(ctx, tx, edge); err != nil {
			return err
		}
		if edge.FromTaskID == edge.ToTaskID {
			return typed(ErrCycleDetectedCode, "self dependency %s", edge.FromTaskID)
		}
		cycle, err := wouldCreateCycle(ctx, tx, opts.cycleDetector, edge.DeliveryRunID, edge.FromTaskID, edge.ToTaskID)
		if err != nil {
			return err
		}
		if cycle {
			return typed(ErrCycleDetectedCode, "edge %s -> %s creates a cycle", edge.FromTaskID, edge.ToTaskID)
		}
		createdBy, err := marshalJSON("dependency edge created_by", edge.CreatedBy)
		if err != nil {
			return err
		}
		host, err := marshalJSON("dependency edge host", edge.Host)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO delivery_dependency_edges(
				edge_id, schema_version, record_version, project_id, delivery_run_id, from_task_id, to_task_id, edge_kind, ordinal,
				plan_fingerprint, created_at, updated_at, created_by_json, host_json
			) VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			edge.EdgeID, edge.SchemaVersion, edge.ProjectID, edge.DeliveryRunID, edge.FromTaskID, edge.ToTaskID, edge.EdgeKind, edge.Ordinal,
			edge.PlanFingerprint, edge.CreatedAt, edge.UpdatedAt, createdBy, host); err != nil {
			if storage.IsConstraint(err) {
				return classifyDependencyEdgeInsertConflict(ctx, tx, opts.IdempotencyKey, edge, request, &out, err)
			}
			return fmt.Errorf("persist dependency edge: %w", err)
		}
		out = edge
		return remember(ctx, tx, opts.IdempotencyKey, edge.ProjectID, edge.DeliveryRunID, "add_dependency_edge", request, out, now)
	})
	return out, err
}

func TransitionDeliveryRun(ctx context.Context, store storage.Store, projectID, deliveryRunID, event string, actor Actor, host Host, opts PersistOptions) (string, error) {
	var next string
	err := withWrite(ctx, store, func(tx storage.Tx) error {
		request := map[string]any{"operation": "transition_delivery_run", "project_id": projectID, "delivery_run_id": deliveryRunID, "event": event, "actor": actor, "host": host}
		replayed, err := replay(ctx, tx, opts.IdempotencyKey, projectID, deliveryRunID, "transition_delivery_run", request, &next)
		if err != nil || replayed {
			return err
		}
		current, err := runState(ctx, tx, projectID, deliveryRunID)
		if err != nil {
			return err
		}
		transitioned, err := DeliveryRunTransition(current, event)
		if err != nil {
			return err
		}
		if transitioned == current {
			next = transitioned
			return remember(ctx, tx, opts.IdempotencyKey, projectID, deliveryRunID, "transition_delivery_run", request, next, optionNow(opts))
		}
		updatedBy, err := marshalJSON("delivery run transition actor", actor)
		if err != nil {
			return err
		}
		hostJSON, err := marshalJSON("delivery run transition host", host)
		if err != nil {
			return err
		}
		now := CanonicalTimestamp(optionNow(opts))
		_, err = tx.Exec(ctx, `UPDATE delivery_runs
			SET state = ?, record_version = record_version + 1, updated_at = ?, updated_by_json = ?, host_json = ?,
				planned_at = CASE WHEN ? IN ('plan_ready_requires_approval', 'plan_ready_no_approval') THEN COALESCE(planned_at, ?) ELSE planned_at END,
				approved_at = CASE WHEN ? = 'approve' THEN COALESCE(approved_at, ?) ELSE approved_at END,
				started_at = CASE WHEN ? = 'start_execution' THEN COALESCE(started_at, ?) ELSE started_at END,
				ended_at = CASE WHEN ? IN ('finish_success', 'finish_failure', 'cancel', 'abandon', 'escalate') AND ? IN ('succeeded', 'failed', 'cancelled', 'abandoned', 'needs-human') THEN COALESCE(ended_at, ?) ELSE ended_at END
			WHERE project_id = ? AND delivery_run_id = ?`,
			transitioned, now, updatedBy, hostJSON, event, now, event, now, event, now, event, transitioned, now, projectID, deliveryRunID)
		if err != nil {
			return fmt.Errorf("transition delivery run: %w", err)
		}
		next = transitioned
		return remember(ctx, tx, opts.IdempotencyKey, projectID, deliveryRunID, "transition_delivery_run", request, next, optionNow(opts))
	})
	if err == nil {
		emitDeliveryRunProgress(ctx, opts.Progress, DeliveryRun{
			ProjectID:     projectID,
			DeliveryRunID: deliveryRunID,
			RunID:         deliveryRunID,
			State:         next,
		}, event, optionNow(opts))
	}
	return next, err
}

func TransitionTask(ctx context.Context, store storage.Store, projectID, deliveryRunID, taskID, event string, actor Actor, host Host, opts PersistOptions) (string, error) {
	var next string
	err := withWrite(ctx, store, func(tx storage.Tx) error {
		request := map[string]any{"operation": "transition_task", "project_id": projectID, "delivery_run_id": deliveryRunID, "task_id": taskID, "event": event, "actor": actor, "host": host}
		replayed, err := replay(ctx, tx, opts.IdempotencyKey, projectID, deliveryRunID, "transition_task", request, &next)
		if err != nil || replayed {
			return err
		}
		current, err := taskState(ctx, tx, projectID, deliveryRunID, taskID)
		if err != nil {
			return err
		}
		transitioned, err := TaskTransition(current, event)
		if err != nil {
			return err
		}
		if transitioned == current {
			next = transitioned
			return remember(ctx, tx, opts.IdempotencyKey, projectID, deliveryRunID, "transition_task", request, next, optionNow(opts))
		}
		updatedBy, err := marshalJSON("task transition actor", actor)
		if err != nil {
			return err
		}
		hostJSON, err := marshalJSON("task transition host", host)
		if err != nil {
			return err
		}
		now := CanonicalTimestamp(optionNow(opts))
		_, err = tx.Exec(ctx, `UPDATE delivery_tasks
			SET state = ?, record_version = record_version + 1, updated_at = ?, updated_by_json = ?, host_json = ?,
				ready_at = CASE WHEN ? IN ('dependencies_ready', 'approval_bound', 'resume') AND ? = 'ready' THEN COALESCE(ready_at, ?) ELSE ready_at END,
				started_at = CASE WHEN ? = 'start' THEN COALESCE(started_at, ?) ELSE started_at END,
				ended_at = CASE WHEN ? IN ('complete_success', 'complete_failure', 'skip', 'cancel', 'escalate') AND ? IN ('succeeded', 'failed', 'skipped', 'cancelled', 'needs-human') THEN COALESCE(ended_at, ?) ELSE ended_at END
			WHERE project_id = ? AND delivery_run_id = ? AND task_id = ?`,
			transitioned, now, updatedBy, hostJSON, event, transitioned, now, event, now, event, transitioned, now, projectID, deliveryRunID, taskID)
		if err != nil {
			return fmt.Errorf("transition task: %w", err)
		}
		next = transitioned
		return remember(ctx, tx, opts.IdempotencyKey, projectID, deliveryRunID, "transition_task", request, next, optionNow(opts))
	})
	if err == nil {
		emitTaskProgress(ctx, opts.Progress, Task{
			ProjectID:     projectID,
			DeliveryRunID: deliveryRunID,
			TaskID:        taskID,
			State:         next,
		}, event, optionNow(opts))
	}
	return next, err
}

func RecordDecision(ctx context.Context, store storage.Store, decision Decision, opts PersistOptions) (Decision, error) {
	if store == nil {
		return Decision{}, typed(ErrInvalidRecordCode, "store is required")
	}
	now := optionNow(opts)
	decision = normalizeDecision(decision, now)
	if err := validateDecision(decision); err != nil {
		return Decision{}, err
	}
	var out Decision
	err := withWrite(ctx, store, func(tx storage.Tx) error {
		if err := ensureRunProject(ctx, tx, decision.ProjectID, decision.DeliveryRunID); err != nil {
			return err
		}
		if decision.TaskID != "" {
			if err := ensureTaskProject(ctx, tx, decision.ProjectID, decision.DeliveryRunID, decision.TaskID); err != nil {
				return err
			}
		}
		request := map[string]any{"operation": "record_decision", "record": decision}
		replayed, err := replay(ctx, tx, opts.IdempotencyKey, decision.ProjectID, decision.DeliveryRunID, "record_decision", request, &out)
		if err != nil || replayed {
			return err
		}
		decidedBy, err := marshalJSON("decision decided_by", decision.DecidedBy)
		if err != nil {
			return err
		}
		createdBy, err := marshalJSON("decision created_by", decision.CreatedBy)
		if err != nil {
			return err
		}
		host, err := marshalJSON("decision host", decision.Host)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO delivery_decisions(
				decision_id, schema_version, record_version, project_id, delivery_run_id, task_id, decision_key, decision_kind,
				decided_by_json, inputs_fingerprint, output_json, alternatives_json, heuristic, heuristic_reason, policy_version,
				side_effect_class, created_at, created_by_json, host_json
			) VALUES (?, ?, 1, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			decision.DecisionID, decision.SchemaVersion, decision.ProjectID, decision.DeliveryRunID, decision.TaskID, decision.DecisionKey, decision.DecisionKind,
			decidedBy, decision.InputsFingerprint, emptyJSON(decision.OutputJSON), nullJSON(decision.AlternativesJSON), boolInt(decision.Heuristic), decision.HeuristicReason,
			decision.PolicyVersion, decision.SideEffectClass, decision.CreatedAt, createdBy, host); err != nil {
			return fmt.Errorf("record decision: %w", err)
		}
		out = decision
		return remember(ctx, tx, opts.IdempotencyKey, decision.ProjectID, decision.DeliveryRunID, "record_decision", request, out, now)
	})
	return out, err
}

func RecordApproval(ctx context.Context, store storage.Store, approval Approval, opts PersistOptions) (Approval, error) {
	if store == nil {
		return Approval{}, typed(ErrInvalidRecordCode, "store is required")
	}
	now := optionNow(opts)
	approval = normalizeApproval(approval, now)
	if err := validateApproval(approval); err != nil {
		return Approval{}, err
	}
	var out Approval
	err := withWrite(ctx, store, func(tx storage.Tx) error {
		request := map[string]any{"operation": "record_approval", "record": approval}
		replayed, err := replay(ctx, tx, opts.IdempotencyKey, approval.ProjectID, approval.DeliveryRunID, "record_approval", request, &out)
		if err != nil || replayed {
			return err
		}
		if err := ensureCurrentFingerprint(ctx, tx, approval.ProjectID, approval.DeliveryRunID, approval.AuthorizationFingerprint, approval.InputFingerprint, approval.PolicyFingerprint, approval.PlanFingerprint); err != nil {
			return err
		}
		approvedBy, err := marshalJSON("approval approved_by", approval.ApprovedBy)
		if err != nil {
			return err
		}
		createdBy, err := marshalJSON("approval created_by", approval.CreatedBy)
		if err != nil {
			return err
		}
		updatedBy, err := marshalJSON("approval updated_by", approval.UpdatedBy)
		if err != nil {
			return err
		}
		host, err := marshalJSON("approval host", approval.Host)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO delivery_approvals(
				approval_id, schema_version, record_version, project_id, delivery_run_id, approval_kind, authorization_fingerprint,
				input_fingerprint, policy_fingerprint, plan_fingerprint, approved_side_effect_class, approved_scope_json,
				approved_by_json, status, approved_at, expires_at, created_at, updated_at, created_by_json, updated_by_json, host_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, ?)`,
			approval.ApprovalID, approval.SchemaVersion, approval.RecordVersion, approval.ProjectID, approval.DeliveryRunID, approval.ApprovalKind, approval.AuthorizationFingerprint,
			approval.InputFingerprint, approval.PolicyFingerprint, approval.PlanFingerprint, approval.ApprovedSideEffectClass, emptyJSON(approval.ApprovedScopeJSON),
			approvedBy, approval.Status, approval.ApprovedAt, approval.ExpiresAt, approval.CreatedAt, approval.UpdatedAt, createdBy, updatedBy, host); err != nil {
			return fmt.Errorf("record approval: %w", err)
		}
		out = approval
		return remember(ctx, tx, opts.IdempotencyKey, approval.ProjectID, approval.DeliveryRunID, "record_approval", request, out, now)
	})
	if err == nil {
		emitApprovalProgress(ctx, opts.Progress, out, "record_approval", optionNow(opts))
	}
	return out, err
}

func RecordOverride(ctx context.Context, store storage.Store, override Override, opts PersistOptions) (Override, error) {
	if store == nil {
		return Override{}, typed(ErrInvalidRecordCode, "store is required")
	}
	now := optionNow(opts)
	override = normalizeOverride(override, now)
	if err := validateOverride(override); err != nil {
		return Override{}, err
	}
	var out Override
	err := withWrite(ctx, store, func(tx storage.Tx) error {
		request := map[string]any{"operation": "record_override", "record": override}
		replayed, err := replay(ctx, tx, opts.IdempotencyKey, override.ProjectID, override.DeliveryRunID, "record_override", request, &out)
		if err != nil || replayed {
			return err
		}
		if err := ensureCurrentFingerprint(ctx, tx, override.ProjectID, override.DeliveryRunID, override.AuthorizationFingerprint, override.InputFingerprint, override.PolicyFingerprint, override.PlanFingerprint); err != nil {
			return err
		}
		if err := ensureDecisionProject(ctx, tx, override.ProjectID, override.DeliveryRunID, override.PolicyDecisionID); err != nil {
			return err
		}
		overriddenBy, err := marshalJSON("override overridden_by", override.OverriddenBy)
		if err != nil {
			return err
		}
		createdBy, err := marshalJSON("override created_by", override.CreatedBy)
		if err != nil {
			return err
		}
		updatedBy, err := marshalJSON("override updated_by", override.UpdatedBy)
		if err != nil {
			return err
		}
		host, err := marshalJSON("override host", override.Host)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO delivery_overrides(
				override_id, schema_version, record_version, project_id, delivery_run_id, override_kind, authorization_fingerprint,
				input_fingerprint, policy_fingerprint, plan_fingerprint, policy_decision_id, overridden_by_json, justification, limits_json, status, created_at,
				updated_at, expires_at, created_by_json, updated_by_json, host_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?)`,
			override.OverrideID, override.SchemaVersion, override.RecordVersion, override.ProjectID, override.DeliveryRunID, override.OverrideKind, override.AuthorizationFingerprint,
			override.InputFingerprint, override.PolicyFingerprint, override.PlanFingerprint, override.PolicyDecisionID, overriddenBy, override.Justification, emptyJSON(override.LimitsJSON), override.Status,
			override.CreatedAt, override.UpdatedAt, override.ExpiresAt, createdBy, updatedBy, host); err != nil {
			return fmt.Errorf("record override: %w", err)
		}
		out = override
		return remember(ctx, tx, opts.IdempotencyKey, override.ProjectID, override.DeliveryRunID, "record_override", request, out, now)
	})
	return out, err
}

func ClaimTask(ctx context.Context, store storage.Store, projectID, deliveryRunID, taskID, executorID string, actor Actor, host Host, opts PersistOptions) (Attempt, error) {
	if store == nil {
		return Attempt{}, typed(ErrInvalidRecordCode, "store is required")
	}
	var out Attempt
	err := withWrite(ctx, store, func(tx storage.Tx) error {
		run, err := loadRunAuthority(ctx, tx, projectID, deliveryRunID)
		if err != nil {
			return err
		}
		if run.State != RunQueued && run.State != RunRunning {
			return typed(ErrInvalidTransitionCode, "delivery run %s is %s", deliveryRunID, run.State)
		}
		task, err := loadTaskAuthority(ctx, tx, projectID, deliveryRunID, taskID)
		if err != nil {
			return err
		}
		if task.State != TaskReady && task.State != TaskClaimed {
			if task.State == TaskAwaitingApproval {
				return typed(ErrApprovalRequiredCode, "task %s is awaiting approval", taskID)
			}
			return typed(ErrInvalidTransitionCode, "task %s is %s", taskID, task.State)
		}
		if err := dependenciesSatisfied(ctx, tx, deliveryRunID, taskID); err != nil {
			return err
		}
		if err := authoritySatisfied(ctx, tx, run, task, optionNow(opts)); err != nil {
			return err
		}
		request := map[string]any{"operation": "claim_task", "project_id": projectID, "delivery_run_id": deliveryRunID, "task_id": taskID, "executor_id": executorID}
		replayed, err := replay(ctx, tx, opts.IdempotencyKey, projectID, deliveryRunID, "claim_task", request, &out)
		if err != nil || replayed {
			return err
		}
		var activeAttempt sql.NullString
		var attemptCount int
		if err := tx.QueryRow(ctx, `SELECT active_attempt_id, attempt_count FROM delivery_tasks WHERE task_id = ?`, taskID).Scan(&activeAttempt, &attemptCount); err != nil {
			return fmt.Errorf("inspect task claim: %w", err)
		}
		if activeAttempt.Valid && strings.TrimSpace(activeAttempt.String) != "" {
			return typed(ErrClaimConflictCode, "task %s already has active attempt %s", taskID, activeAttempt.String)
		}
		now := CanonicalTimestamp(optionNow(opts))
		ordinal := attemptCount + 1
		createdBy, err := marshalJSON("attempt created_by", actor)
		if err != nil {
			return err
		}
		hostJSON, err := marshalJSON("attempt host", host)
		if err != nil {
			return err
		}
		out = Attempt{
			SchemaVersion:          SchemaAttempt,
			RecordVersion:          1,
			AttemptID:              stableID("att", taskID, fmt.Sprint(ordinal)),
			TaskID:                 taskID,
			DeliveryRunID:          deliveryRunID,
			ProjectID:              projectID,
			AttemptOrdinal:         ordinal,
			State:                  AttemptClaimed,
			ClaimGeneration:        int64(ordinal),
			ExecutorID:             strings.TrimSpace(executorID),
			ProviderIdempotencyKey: "task-attempt:" + taskID + ":" + fmt.Sprint(ordinal),
			SideEffectClass:        task.SideEffectClass,
			CreatedAt:              now,
			UpdatedAt:              now,
			CreatedBy:              actor,
			UpdatedBy:              actor,
			Host:                   host,
		}
		if _, err := tx.Exec(ctx, `INSERT INTO delivery_attempts(
				attempt_id, schema_version, record_version, project_id, delivery_run_id, task_id, attempt_ordinal, state,
				claim_generation, executor_id, provider_idempotency_key, side_effect_class, created_at, updated_at,
				created_by_json, updated_by_json, host_json
			) VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			out.AttemptID, out.SchemaVersion, projectID, deliveryRunID, taskID, ordinal, out.State, out.ClaimGeneration, out.ExecutorID,
			out.ProviderIdempotencyKey, out.SideEffectClass, now, now, createdBy, createdBy, hostJSON); err != nil {
			return fmt.Errorf("claim task attempt: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE delivery_tasks SET state = 'claimed', active_attempt_id = ?, attempt_count = ?, updated_at = ?, updated_by_json = ?, host_json = ? WHERE task_id = ?`,
			out.AttemptID, ordinal, now, createdBy, hostJSON, taskID); err != nil {
			return fmt.Errorf("claim task update: %w", err)
		}
		return remember(ctx, tx, opts.IdempotencyKey, projectID, deliveryRunID, "claim_task", request, out, optionNow(opts))
	})
	if err == nil {
		emitAttemptProgress(ctx, opts.Progress, out, "claim_task", optionNow(opts))
	}
	return out, err
}
