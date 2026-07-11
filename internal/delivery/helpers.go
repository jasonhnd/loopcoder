package delivery

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/storage"
)

type runAuthority struct {
	ProjectID                string
	DeliveryRunID            string
	State                    string
	AuthorizationFingerprint string
	InputFingerprint         string
	PolicyFingerprint        string
	PlanFingerprint          string
	MaxSideEffectClass       string
}

type taskAuthority struct {
	ProjectID                string
	DeliveryRunID            string
	TaskID                   string
	State                    string
	AuthorizationFingerprint string
	SideEffectClass          string
}

var cycleDetectionErrorForTest error

func withWrite(ctx context.Context, store storage.Store, fn func(storage.Tx) error) error {
	if store == nil {
		return typed(ErrInvalidRecordCode, "store is required")
	}
	return store.WithWriteTx(ctx, fn)
}

func optionNow(opts PersistOptions) time.Time {
	if opts.Now.IsZero() {
		return time.Now().UTC()
	}
	return opts.Now.UTC()
}

func normalizeDeliveryRun(run DeliveryRun, now time.Time) DeliveryRun {
	if run.SchemaVersion == "" {
		run.SchemaVersion = SchemaDeliveryRun
	}
	if run.RecordVersion == 0 {
		run.RecordVersion = 1
	}
	run.DeliveryRunID = strings.TrimSpace(run.DeliveryRunID)
	if run.RunID == "" {
		run.RunID = run.DeliveryRunID
	}
	if run.RootRunID == "" {
		run.RootRunID = run.DeliveryRunID
	}
	if run.State == "" {
		run.State = RunDraft
	}
	if run.MaxSideEffectClass == "" {
		run.MaxSideEffectClass = "none"
	}
	if run.ApprovalStatus == "" {
		run.ApprovalStatus = "not-required"
	}
	if run.OverrideStatus == "" {
		run.OverrideStatus = "none"
	}
	if run.PolicyVersion == "" {
		run.PolicyVersion = "unspecified"
	}
	nowText := CanonicalTimestamp(now)
	if run.CreatedAt == "" {
		run.CreatedAt = nowText
	}
	if run.UpdatedAt == "" {
		run.UpdatedAt = run.CreatedAt
	}
	return run
}

func normalizePlanFingerprint(fp PlanFingerprint, now time.Time) PlanFingerprint {
	if fp.SchemaVersion == "" {
		fp.SchemaVersion = SchemaPlanFingerprint
	}
	if fp.RecordVersion == 0 {
		fp.RecordVersion = 1
	}
	if fp.CanonicalizationVersion == "" {
		fp.CanonicalizationVersion = CanonicalJSONVersion
	}
	if fp.Algorithm == "" {
		fp.Algorithm = "sha256"
	}
	if fp.FingerprintID == "" {
		fp.FingerprintID = fp.PlanFingerprint
	}
	if fp.CreatedAt == "" {
		fp.CreatedAt = CanonicalTimestamp(now)
	}
	return fp
}

func normalizeTask(task Task, now time.Time) Task {
	if task.SchemaVersion == "" {
		task.SchemaVersion = SchemaTask
	}
	if task.RecordVersion == 0 {
		task.RecordVersion = 1
	}
	if task.State == "" {
		task.State = TaskPending
	}
	if task.RequirementsJSON == "" {
		task.RequirementsJSON = "{}"
	}
	if task.ScopeJSON == "" {
		task.ScopeJSON = "{}"
	}
	if task.Permission == "" {
		task.Permission = "read-only"
	}
	if task.SideEffectClass == "" {
		task.SideEffectClass = "none"
	}
	if task.PolicyVersion == "" {
		task.PolicyVersion = "unspecified"
	}
	if task.TaskID == "" && task.ProjectID != "" && task.DeliveryRunID != "" && task.TaskKey != "" {
		task.TaskID = stableID("task", task.ProjectID, task.DeliveryRunID, task.TaskKey)
	}
	nowText := CanonicalTimestamp(now)
	if task.CreatedAt == "" {
		task.CreatedAt = nowText
	}
	if task.UpdatedAt == "" {
		task.UpdatedAt = task.CreatedAt
	}
	return task
}

func normalizeEdge(edge DependencyEdge, now time.Time) DependencyEdge {
	if edge.SchemaVersion == "" {
		edge.SchemaVersion = SchemaDependencyEdge
	}
	if edge.RecordVersion == 0 {
		edge.RecordVersion = 1
	}
	if edge.EdgeKind == "" {
		edge.EdgeKind = "requires"
	}
	if edge.EdgeID == "" {
		edge.EdgeID = stableID("edge", edge.ProjectID, edge.DeliveryRunID, edge.FromTaskID, edge.ToTaskID, edge.EdgeKind)
	}
	nowText := CanonicalTimestamp(now)
	if edge.CreatedAt == "" {
		edge.CreatedAt = nowText
	}
	if edge.UpdatedAt == "" {
		edge.UpdatedAt = edge.CreatedAt
	}
	return edge
}

func normalizeDecision(decision Decision, now time.Time) Decision {
	if decision.SchemaVersion == "" {
		decision.SchemaVersion = SchemaDecision
	}
	if decision.RecordVersion == 0 {
		decision.RecordVersion = 1
	}
	if decision.DecisionID == "" {
		decision.DecisionID = stableID("dec", decision.ProjectID, decision.DeliveryRunID, decision.DecisionKey, decision.CreatedAt)
	}
	if decision.OutputJSON == "" {
		decision.OutputJSON = "{}"
	}
	if decision.AlternativesJSON == "" {
		decision.AlternativesJSON = "null"
	}
	if decision.SideEffectClass == "" {
		decision.SideEffectClass = "none"
	}
	if decision.PolicyVersion == "" {
		decision.PolicyVersion = "unspecified"
	}
	if decision.CreatedAt == "" {
		decision.CreatedAt = CanonicalTimestamp(now)
	}
	if strings.TrimSpace(decision.DecisionID) == "dec_" {
		decision.DecisionID = stableID("dec", decision.ProjectID, decision.DeliveryRunID, decision.DecisionKey, decision.CreatedAt)
	}
	return decision
}

func normalizeApproval(approval Approval, now time.Time) Approval {
	if approval.SchemaVersion == "" {
		approval.SchemaVersion = SchemaApproval
	}
	if approval.RecordVersion == 0 {
		approval.RecordVersion = 1
	}
	if approval.Status == "" {
		approval.Status = "active"
	}
	if approval.ApprovedScopeJSON == "" {
		approval.ApprovedScopeJSON = "{}"
	}
	if approval.ApprovalID == "" {
		approval.ApprovalID = stableID("appr", approval.ProjectID, approval.DeliveryRunID, approval.AuthorizationFingerprint, approval.ApprovedBy.ActorID, approval.CreatedAt)
	}
	nowText := CanonicalTimestamp(now)
	if approval.CreatedAt == "" {
		approval.CreatedAt = nowText
	}
	if approval.UpdatedAt == "" {
		approval.UpdatedAt = approval.CreatedAt
	}
	if approval.Status == "active" && approval.ApprovedAt == "" {
		approval.ApprovedAt = approval.CreatedAt
	}
	return approval
}

func normalizeOverride(override Override, now time.Time) Override {
	if override.SchemaVersion == "" {
		override.SchemaVersion = SchemaOverride
	}
	if override.RecordVersion == 0 {
		override.RecordVersion = 1
	}
	if override.Status == "" {
		override.Status = "active"
	}
	if override.LimitsJSON == "" {
		override.LimitsJSON = "{}"
	}
	if override.OverrideID == "" {
		override.OverrideID = stableID("ovr", override.ProjectID, override.DeliveryRunID, override.AuthorizationFingerprint, override.OverrideKind, override.OverriddenBy.ActorID, override.CreatedAt)
	}
	nowText := CanonicalTimestamp(now)
	if override.CreatedAt == "" {
		override.CreatedAt = nowText
	}
	if override.UpdatedAt == "" {
		override.UpdatedAt = override.CreatedAt
	}
	return override
}

func validateDeliveryRun(run DeliveryRun) error {
	if run.SchemaVersion != SchemaDeliveryRun || run.RecordVersion < 1 || run.ProjectID == "" || run.DeliveryRunID == "" || run.RootRunID == "" || run.State == "" || run.IntentSummary == "" {
		return typed(ErrInvalidRecordCode, "delivery run has missing required fields")
	}
	if _, ok := runTransitions[run.State]; !ok {
		return typed(ErrInvalidRecordCode, "unknown delivery run state %q", run.State)
	}
	return validateTimestamps(run.CreatedAt, run.UpdatedAt, run.PlannedAt, run.ApprovedAt, run.StartedAt, run.EndedAt)
}

func validatePlanFingerprint(fp PlanFingerprint) error {
	if fp.SchemaVersion != SchemaPlanFingerprint || fp.RecordVersion != 1 || fp.ProjectID == "" || fp.DeliveryRunID == "" || fp.FingerprintID == "" || fp.InputFingerprint == "" || fp.PolicyFingerprint == "" || fp.PlanFingerprint == "" || fp.AuthorizationFingerprint == "" {
		return typed(ErrInvalidRecordCode, "plan fingerprint has missing required fields")
	}
	if fp.Algorithm != "sha256" || fp.CanonicalizationVersion != CanonicalJSONVersion {
		return typed(ErrInvalidRecordCode, "unsupported fingerprint algorithm or canonicalization")
	}
	return validateTimestamps(fp.CreatedAt)
}

func validateTask(task Task) error {
	if task.SchemaVersion != SchemaTask || task.RecordVersion < 1 || task.ProjectID == "" || task.DeliveryRunID == "" || task.TaskID == "" || task.TaskKey == "" || task.Title == "" || task.PlanFingerprint == "" {
		return typed(ErrInvalidRecordCode, "task has missing required fields")
	}
	if _, ok := taskTransitions[task.State]; !ok {
		return typed(ErrInvalidRecordCode, "unknown task state %q", task.State)
	}
	if !json.Valid([]byte(task.RequirementsJSON)) || !json.Valid([]byte(task.ScopeJSON)) {
		return typed(ErrInvalidRecordCode, "task JSON payload is invalid")
	}
	return validateTimestamps(task.CreatedAt, task.UpdatedAt, task.ReadyAt, task.StartedAt, task.EndedAt)
}

func validateEdge(edge DependencyEdge) error {
	if edge.SchemaVersion != SchemaDependencyEdge || edge.ProjectID == "" || edge.DeliveryRunID == "" || edge.EdgeID == "" || edge.FromTaskID == "" || edge.ToTaskID == "" || edge.PlanFingerprint == "" {
		return typed(ErrInvalidRecordCode, "dependency edge has missing required fields")
	}
	switch edge.EdgeKind {
	case "requires", "blocks", "orders-after":
	default:
		return typed(ErrInvalidRecordCode, "unknown edge kind %q", edge.EdgeKind)
	}
	return validateTimestamps(edge.CreatedAt, edge.UpdatedAt)
}

func validateDecision(decision Decision) error {
	if decision.SchemaVersion != SchemaDecision || decision.ProjectID == "" || decision.DeliveryRunID == "" || decision.DecisionID == "" || decision.DecisionKey == "" || decision.DecisionKind == "" || decision.InputsFingerprint == "" {
		return typed(ErrInvalidRecordCode, "decision has missing required fields")
	}
	if decision.Heuristic && strings.TrimSpace(decision.HeuristicReason) == "" {
		return typed(ErrInvalidRecordCode, "heuristic decision requires heuristic_reason")
	}
	if !json.Valid([]byte(decision.OutputJSON)) || !json.Valid([]byte(nullJSON(decision.AlternativesJSON))) {
		return typed(ErrInvalidRecordCode, "decision JSON payload is invalid")
	}
	return validateTimestamps(decision.CreatedAt)
}

func validateApproval(approval Approval) error {
	if approval.SchemaVersion != SchemaApproval || approval.ProjectID == "" || approval.DeliveryRunID == "" || approval.ApprovalID == "" || approval.AuthorizationFingerprint == "" || approval.InputFingerprint == "" || approval.PolicyFingerprint == "" || approval.PlanFingerprint == "" || approval.ApprovedSideEffectClass == "" {
		return typed(ErrInvalidRecordCode, "approval has missing required fields")
	}
	if approval.Status == "active" && approval.ApprovedAt == "" {
		return typed(ErrInvalidRecordCode, "active approval requires approved_at")
	}
	if !json.Valid([]byte(approval.ApprovedScopeJSON)) {
		return typed(ErrInvalidRecordCode, "approved_scope_json is invalid")
	}
	return validateTimestamps(approval.CreatedAt, approval.UpdatedAt, approval.ApprovedAt, approval.ExpiresAt)
}

func validateOverride(override Override) error {
	if override.SchemaVersion != SchemaOverride || override.ProjectID == "" || override.DeliveryRunID == "" || override.OverrideID == "" || override.OverrideKind == "" || override.AuthorizationFingerprint == "" || override.InputFingerprint == "" || override.PolicyFingerprint == "" || override.PlanFingerprint == "" || override.PolicyDecisionID == "" || override.Justification == "" {
		return typed(ErrInvalidRecordCode, "override has missing required fields")
	}
	switch override.OverrideKind {
	case "budget", "scope", "side-effect-class", "recovery":
		if override.ExpiresAt == "" {
			return typed(ErrInvalidRecordCode, "%s override requires expires_at", override.OverrideKind)
		}
	}
	if !json.Valid([]byte(override.LimitsJSON)) {
		return typed(ErrInvalidRecordCode, "limits_json is invalid")
	}
	return validateTimestamps(override.CreatedAt, override.UpdatedAt, override.ExpiresAt)
}

func validateTimestamps(values ...string) error {
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
			return typed(ErrInvalidRecordCode, "invalid timestamp %q", value)
		}
	}
	return nil
}

func ensureRunProject(ctx context.Context, tx storage.Tx, projectID, deliveryRunID string) error {
	var found string
	err := tx.QueryRow(ctx, `SELECT project_id FROM delivery_runs WHERE delivery_run_id = ?`, deliveryRunID).Scan(&found)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return typed(ErrMissingReferenceCode, "delivery run %s is missing", deliveryRunID)
		}
		return fmt.Errorf("inspect delivery run: %w", err)
	}
	if found != projectID {
		return typed(ErrCrossProjectReferenceCode, "delivery run %s belongs to %s", deliveryRunID, found)
	}
	return nil
}

func ensureTaskProject(ctx context.Context, tx storage.Tx, projectID, deliveryRunID, taskID string) error {
	var foundProject, foundRun string
	err := tx.QueryRow(ctx, `SELECT project_id, delivery_run_id FROM delivery_tasks WHERE task_id = ?`, taskID).Scan(&foundProject, &foundRun)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return typed(ErrMissingReferenceCode, "task %s is missing", taskID)
		}
		return fmt.Errorf("inspect task: %w", err)
	}
	if foundProject != projectID || foundRun != deliveryRunID {
		return typed(ErrCrossProjectReferenceCode, "task %s belongs to %s/%s", taskID, foundProject, foundRun)
	}
	return nil
}

func ensureDecisionProject(ctx context.Context, tx storage.Tx, projectID, deliveryRunID, decisionID string) error {
	var foundProject, foundRun string
	err := tx.QueryRow(ctx, `SELECT project_id, delivery_run_id FROM delivery_decisions WHERE decision_id = ?`, decisionID).Scan(&foundProject, &foundRun)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return typed(ErrMissingReferenceCode, "decision %s is missing", decisionID)
		}
		return fmt.Errorf("inspect decision: %w", err)
	}
	if foundProject != projectID || foundRun != deliveryRunID {
		return typed(ErrCrossProjectReferenceCode, "decision %s belongs to %s/%s", decisionID, foundProject, foundRun)
	}
	return nil
}

func taskByKey(ctx context.Context, tx storage.Tx, projectID, runID, taskKey string) (string, bool, error) {
	var taskID string
	err := tx.QueryRow(ctx, `SELECT task_id FROM delivery_tasks WHERE project_id = ? AND delivery_run_id = ? AND task_key = ?`, projectID, runID, taskKey).Scan(&taskID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("inspect task key: %w", err)
	}
	return taskID, true, nil
}

func runState(ctx context.Context, tx storage.Tx, projectID, runID string) (string, error) {
	var state string
	err := tx.QueryRow(ctx, `SELECT state FROM delivery_runs WHERE project_id = ? AND delivery_run_id = ?`, projectID, runID).Scan(&state)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", typed(ErrMissingReferenceCode, "delivery run %s is missing", runID)
		}
		return "", fmt.Errorf("inspect delivery run state: %w", err)
	}
	return state, nil
}

func taskState(ctx context.Context, tx storage.Tx, projectID, runID, taskID string) (string, error) {
	var state string
	err := tx.QueryRow(ctx, `SELECT state FROM delivery_tasks WHERE project_id = ? AND delivery_run_id = ? AND task_id = ?`, projectID, runID, taskID).Scan(&state)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", typed(ErrMissingReferenceCode, "task %s is missing", taskID)
		}
		return "", fmt.Errorf("inspect task state: %w", err)
	}
	return state, nil
}

func loadRunAuthority(ctx context.Context, tx storage.Tx, projectID, runID string) (runAuthority, error) {
	var run runAuthority
	err := tx.QueryRow(ctx, `SELECT project_id, delivery_run_id, state, authorization_fingerprint, input_fingerprint, policy_fingerprint, plan_fingerprint, max_side_effect_class FROM delivery_runs WHERE project_id = ? AND delivery_run_id = ?`, projectID, runID).Scan(&run.ProjectID, &run.DeliveryRunID, &run.State, &run.AuthorizationFingerprint, &run.InputFingerprint, &run.PolicyFingerprint, &run.PlanFingerprint, &run.MaxSideEffectClass)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return runAuthority{}, typed(ErrMissingReferenceCode, "delivery run %s is missing", runID)
		}
		return runAuthority{}, fmt.Errorf("inspect run authority: %w", err)
	}
	return run, nil
}

func loadTaskAuthority(ctx context.Context, tx storage.Tx, projectID, runID, taskID string) (taskAuthority, error) {
	var task taskAuthority
	err := tx.QueryRow(ctx, `SELECT project_id, delivery_run_id, task_id, state, authorization_fingerprint, side_effect_class FROM delivery_tasks WHERE project_id = ? AND delivery_run_id = ? AND task_id = ?`, projectID, runID, taskID).Scan(&task.ProjectID, &task.DeliveryRunID, &task.TaskID, &task.State, &task.AuthorizationFingerprint, &task.SideEffectClass)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return taskAuthority{}, typed(ErrMissingReferenceCode, "task %s is missing", taskID)
		}
		return taskAuthority{}, fmt.Errorf("inspect task authority: %w", err)
	}
	return task, nil
}

func wouldCreateCycle(ctx context.Context, tx storage.Tx, runID, fromTaskID, toTaskID string) (bool, error) {
	if cycleDetectionErrorForTest != nil {
		return false, fmt.Errorf("inspect dependency cycle: %w", cycleDetectionErrorForTest)
	}
	rows, err := tx.Query(ctx, `WITH RECURSIVE walk(task_id) AS (
		SELECT to_task_id FROM delivery_dependency_edges WHERE delivery_run_id = ? AND from_task_id = ?
		UNION
		SELECT e.to_task_id FROM delivery_dependency_edges e JOIN walk ON e.from_task_id = walk.task_id WHERE e.delivery_run_id = ?
	) SELECT task_id FROM walk`, runID, toTaskID, runID)
	if err != nil {
		return false, fmt.Errorf("inspect dependency cycle: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return false, fmt.Errorf("inspect dependency cycle row: %w", err)
		}
		if id == fromTaskID {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("inspect dependency cycle rows: %w", err)
	}
	return false, nil
}

func dependenciesSatisfied(ctx context.Context, tx storage.Tx, runID, taskID string) error {
	var blocked int
	err := tx.QueryRow(ctx, `SELECT COUNT(*)
		FROM delivery_dependency_edges e
		JOIN delivery_tasks t ON t.task_id = e.from_task_id
		WHERE e.delivery_run_id = ? AND e.to_task_id = ? AND t.state <> 'succeeded'`, runID, taskID).Scan(&blocked)
	if err != nil {
		return fmt.Errorf("inspect task dependencies: %w", err)
	}
	if blocked > 0 {
		return typed(ErrInvalidTransitionCode, "task %s has unsatisfied dependencies", taskID)
	}
	return nil
}

func authoritySatisfied(ctx context.Context, tx storage.Tx, run runAuthority, task taskAuthority, now time.Time) error {
	auth := firstNonEmpty(task.AuthorizationFingerprint, run.AuthorizationFingerprint)
	if auth == "" {
		return nil
	}
	if sideEffectRank(task.SideEffectClass) > sideEffectRank(run.MaxSideEffectClass) {
		return typed(ErrSideEffectClassExceededCode, "task side effect %s exceeds run maximum %s", task.SideEffectClass, run.MaxSideEffectClass)
	}
	var expires string
	err := tx.QueryRow(ctx, `SELECT COALESCE(expires_at, '')
		FROM delivery_approvals
		WHERE project_id = ? AND delivery_run_id = ? AND authorization_fingerprint = ? AND status = 'active'
		ORDER BY approved_at DESC LIMIT 1`, run.ProjectID, run.DeliveryRunID, auth).Scan(&expires)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return typed(ErrApprovalRequiredCode, "approval for %s is required", auth)
		}
		return fmt.Errorf("inspect approval: %w", err)
	}
	if expires != "" {
		expiry, err := time.Parse(time.RFC3339Nano, expires)
		if err != nil {
			return typed(ErrInvalidRecordCode, "approval expiry is invalid")
		}
		if !expiry.After(now) {
			return typed(ErrExpiredApprovalCode, "approval for %s expired at %s", auth, expires)
		}
	}
	return nil
}

func ensureCurrentFingerprint(ctx context.Context, tx storage.Tx, projectID, runID, auth, input, policy, plan string) error {
	var storedInput, storedPolicy, storedPlan string
	err := tx.QueryRow(ctx, `SELECT input_fingerprint, policy_fingerprint, plan_fingerprint FROM delivery_runs WHERE project_id = ? AND delivery_run_id = ? AND authorization_fingerprint = ?`, projectID, runID, auth).Scan(&storedInput, &storedPolicy, &storedPlan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return typed(ErrStaleApprovalCode, "authorization fingerprint does not match current run")
		}
		return fmt.Errorf("inspect run fingerprint: %w", err)
	}
	if storedInput != input || storedPolicy != policy || storedPlan != plan {
		return typed(ErrStaleApprovalCode, "approval fingerprint inputs do not match current run")
	}
	return nil
}

func ensureDependencyEdgeAbsent(ctx context.Context, tx storage.Tx, edge DependencyEdge) error {
	var existing string
	err := tx.QueryRow(ctx, `SELECT edge_id FROM delivery_dependency_edges WHERE delivery_run_id = ? AND from_task_id = ? AND to_task_id = ? AND edge_kind = ?`,
		edge.DeliveryRunID, edge.FromTaskID, edge.ToTaskID, edge.EdgeKind).Scan(&existing)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("inspect dependency edge duplicate: %w", err)
	}
	return typed(ErrDuplicateRecordCode, "dependency edge already exists as %s", existing)
}

func replay(ctx context.Context, tx storage.Tx, key, projectID, runID, op string, request any, out any) (bool, error) {
	key, err := idempotencyKey(key, op, request)
	if err != nil {
		return false, err
	}
	requestBytes, err := CanonicalJSON(request)
	if err != nil {
		return false, err
	}
	requestHash := SHA256Digest(requestBytes)
	var storedHash, resultJSON string
	err = tx.QueryRow(ctx, `SELECT request_hash, result_json FROM delivery_idempotency WHERE idempotency_key = ?`, key).Scan(&storedHash, &resultJSON)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("inspect idempotency key: %w", err)
	}
	if storedHash != requestHash {
		return false, typed(ErrDuplicateReplayCode, "idempotency key %s replayed with different request", key)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(resultJSON), out); err != nil {
			return false, typed(ErrInvalidRecordCode, "idempotency result is invalid")
		}
	}
	return true, nil
}

func remember(ctx context.Context, tx storage.Tx, key, projectID, runID, op string, request any, result any, now time.Time) error {
	key, err := idempotencyKey(key, op, request)
	if err != nil {
		return err
	}
	requestBytes, err := CanonicalJSON(request)
	if err != nil {
		return err
	}
	resultBytes, err := json.Marshal(result)
	if err != nil {
		return typed(ErrInvalidRecordCode, "marshal idempotency result: %v", err)
	}
	_, err = tx.Exec(ctx, `INSERT OR IGNORE INTO delivery_idempotency(idempotency_key, project_id, delivery_run_id, operation, request_hash, request_json, result_json, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		key, projectID, runID, op, SHA256Digest(requestBytes), string(requestBytes), string(resultBytes), CanonicalTimestamp(now))
	if err != nil {
		return fmt.Errorf("record idempotency key: %w", err)
	}
	return nil
}

func idempotencyKey(key, op string, request any) (string, error) {
	key = strings.TrimSpace(key)
	if key != "" {
		return key, nil
	}
	bytes, err := CanonicalJSON(request)
	if err != nil {
		return "", fmt.Errorf("derive idempotency key for %s: %w", op, err)
	}
	return "idem_" + digestBase32(op, string(bytes))[:32], nil
}

func marshalJSON(label string, value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshal %s: %w", label, err)
	}
	return string(data), nil
}

func stableID(prefix string, parts ...string) string {
	return prefix + "_" + digestBase32(parts...)[:32]
}

func digestBase32(parts ...string) string {
	sum := sha256.New()
	for _, part := range parts {
		sum.Write([]byte(part))
		sum.Write([]byte{0})
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum.Sum(nil))
	return strings.ToLower(encoded)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func emptyJSON(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "{}"
	}
	return value
}

func nullJSON(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "null"
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func sideEffectRank(value string) int {
	switch strings.TrimSpace(value) {
	case "none":
		return 0
	case "local-read":
		return 1
	case "local-write":
		return 2
	case "repo-write":
		return 3
	case "git-remote-write":
		return 4
	case "github-write":
		return 5
	case "provider-launch":
		return 6
	case "external-write":
		return 7
	default:
		return 100
	}
}
