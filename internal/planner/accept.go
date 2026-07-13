package planner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

const SchemaAcceptedTaskGraphVersion = "loopcoder.accepted_task_graph_version.v1"

type AcceptOptions struct {
	ExpectedAuthorizationFingerprint string
	IdempotencyKey                   string
	Actor                            delivery.Actor
	Host                             delivery.Host
	Now                              time.Time

	afterRunPersistedForTest func() error
}

type AcceptedGraphVersion struct {
	SchemaVersion                   string         `json:"schema_version"`
	RecordVersion                   int            `json:"record_version"`
	GraphVersionID                  string         `json:"graph_version_id"`
	ProjectID                       string         `json:"project_id"`
	DeliveryRunID                   string         `json:"delivery_run_id"`
	InputFingerprint                string         `json:"input_fingerprint"`
	PolicyFingerprint               string         `json:"policy_fingerprint"`
	PlanFingerprint                 string         `json:"plan_fingerprint"`
	AuthorizationFingerprint        string         `json:"authorization_fingerprint"`
	PolicyVersion                   string         `json:"policy_version"`
	RoutingPolicyProfileID          string         `json:"routing_policy_profile_id"`
	RoutingPolicyProfileFingerprint string         `json:"routing_policy_profile_fingerprint"`
	TaskGraphValidationID           string         `json:"task_graph_validation_id"`
	ApprovalID                      string         `json:"approval_id,omitempty"`
	TaskCount                       int            `json:"task_count"`
	EdgeCount                       int            `json:"edge_count"`
	GraphFingerprint                string         `json:"graph_fingerprint"`
	CanonicalProposalHash           string         `json:"canonical_proposal_hash"`
	AcceptedAt                      string         `json:"accepted_at"`
	AcceptedBy                      delivery.Actor `json:"accepted_by"`
	Host                            delivery.Host  `json:"host"`
	Proposal                        GraphProposal  `json:"proposal"`
}

func AcceptGraphProposal(ctx context.Context, store storage.Store, proposal GraphProposal, opts AcceptOptions) (AcceptedGraphVersion, error) {
	if store == nil {
		return AcceptedGraphVersion{}, taskErr(taskrequirements.ErrInvalidRecordCode, "store is required")
	}
	if opts.Now.IsZero() {
		return AcceptedGraphVersion{}, taskErr(taskrequirements.ErrInvalidRecordCode, "acceptance now is required")
	}
	if strings.TrimSpace(opts.ExpectedAuthorizationFingerprint) == "" {
		return AcceptedGraphVersion{}, taskErr(taskrequirements.ErrInvalidRecordCode, "expected authorization fingerprint is required")
	}
	if opts.ExpectedAuthorizationFingerprint != proposal.AuthorizationFingerprint {
		return AcceptedGraphVersion{}, fmt.Errorf("%w: expected authorization fingerprint does not match proposed graph", delivery.ErrStaleApproval)
	}
	if err := validateProposalAtAuthorityBoundary(proposal); err != nil {
		return AcceptedGraphVersion{}, err
	}
	canonicalProposal, err := delivery.CanonicalJSON(proposal)
	if err != nil {
		return AcceptedGraphVersion{}, taskErr(taskrequirements.ErrInvalidRecordCode, "canonical proposal: %v", err)
	}
	proposalHash := delivery.SHA256Digest(canonicalProposal)
	graphFingerprint, _, err := delivery.DigestCanonicalJSON(map[string]any{
		"schema_version":            SchemaAcceptedTaskGraphVersion,
		"project_id":                proposal.ProjectID,
		"delivery_run_id":           proposal.DeliveryRunID,
		"input_fingerprint":         proposal.InputFingerprint,
		"policy_fingerprint":        proposal.PolicyFingerprint,
		"plan_fingerprint":          proposal.PlanFingerprint,
		"authorization_fingerprint": proposal.AuthorizationFingerprint,
		"policy_version":            proposal.PolicyVersion,
		"canonical_proposal_hash":   proposalHash,
	})
	if err != nil {
		return AcceptedGraphVersion{}, err
	}
	now := opts.Now.UTC()
	version := AcceptedGraphVersion{
		SchemaVersion:                   SchemaAcceptedTaskGraphVersion,
		RecordVersion:                   1,
		GraphVersionID:                  stableID("tgraph", proposal.ProjectID, proposal.DeliveryRunID, proposal.AuthorizationFingerprint),
		ProjectID:                       proposal.ProjectID,
		DeliveryRunID:                   proposal.DeliveryRunID,
		InputFingerprint:                proposal.InputFingerprint,
		PolicyFingerprint:               proposal.PolicyFingerprint,
		PlanFingerprint:                 proposal.PlanFingerprint,
		AuthorizationFingerprint:        proposal.AuthorizationFingerprint,
		PolicyVersion:                   proposal.PolicyVersion,
		RoutingPolicyProfileID:          proposal.RoutingPolicyProfileID,
		RoutingPolicyProfileFingerprint: proposal.RoutingPolicyProfileFingerprint,
		TaskGraphValidationID:           proposal.Validation.TaskGraphValidationID,
		TaskCount:                       len(proposal.Tasks),
		EdgeCount:                       len(proposal.Edges),
		GraphFingerprint:                graphFingerprint,
		CanonicalProposalHash:           proposalHash,
		AcceptedAt:                      delivery.CanonicalTimestamp(now),
		AcceptedBy:                      opts.Actor,
		Host:                            opts.Host,
		Proposal:                        proposal,
	}
	if proposal.ApprovalRequirement == "required" {
		version.ApprovalID = stableID("appr", proposal.ProjectID, proposal.DeliveryRunID, proposal.AuthorizationFingerprint, opts.Actor.ActorID)
	}

	var out AcceptedGraphVersion
	err = store.WithWriteTx(ctx, func(tx storage.Tx) error {
		request := map[string]any{"operation": "accept_task_graph", "proposal": proposal, "actor": opts.Actor}
		replayed, err := replayAcceptedGraph(ctx, tx, opts.IdempotencyKey, proposal.ProjectID, proposal.DeliveryRunID, request, &out)
		if err != nil || replayed {
			return err
		}
		existing, ok, err := loadAcceptedGraphVersionInTx(ctx, tx, proposal.ProjectID, proposal.DeliveryRunID)
		if err != nil {
			return err
		}
		if ok {
			if existing.AuthorizationFingerprint == proposal.AuthorizationFingerprint && existing.CanonicalProposalHash == proposalHash {
				out = existing
				return rememberAcceptedGraph(ctx, tx, opts.IdempotencyKey, proposal.ProjectID, proposal.DeliveryRunID, request, out, now)
			}
			return fmt.Errorf("%w: delivery run already has an immutable accepted graph version", delivery.ErrStaleApproval)
		}
		if err := ensureProjectExists(ctx, tx, proposal.ProjectID); err != nil {
			return err
		}
		if err := ensureRunAcceptable(ctx, tx, proposal); err != nil {
			return err
		}
		if err := persistAcceptedRun(ctx, tx, proposal, opts, now); err != nil {
			return err
		}
		if opts.afterRunPersistedForTest != nil {
			if err := opts.afterRunPersistedForTest(); err != nil {
				return err
			}
		}
		if err := persistAcceptedPlanFingerprint(ctx, tx, proposal, opts, now); err != nil {
			return err
		}
		for _, task := range proposal.Tasks {
			if err := persistAcceptedTask(ctx, tx, task.Task, proposal.AuthorizationFingerprint); err != nil {
				return err
			}
			if err := persistAcceptedRequirement(ctx, tx, task.Requirement); err != nil {
				return err
			}
		}
		for _, edge := range proposal.Edges {
			if err := persistAcceptedEdge(ctx, tx, edge); err != nil {
				return err
			}
		}
		if version.ApprovalID != "" {
			if err := persistAcceptedApproval(ctx, tx, proposal, version.ApprovalID, opts, now); err != nil {
				return err
			}
		}
		if err := persistGraphValidation(ctx, tx, proposal, opts, canonicalProposal, now); err != nil {
			return err
		}
		if err := persistGraphVersion(ctx, tx, version, canonicalProposal); err != nil {
			return err
		}
		out = version
		return rememberAcceptedGraph(ctx, tx, opts.IdempotencyKey, proposal.ProjectID, proposal.DeliveryRunID, request, out, now)
	})
	return out, err
}

func LoadAcceptedGraphVersion(ctx context.Context, store storage.Store, projectID, deliveryRunID string) (AcceptedGraphVersion, error) {
	if store == nil {
		return AcceptedGraphVersion{}, taskErr(taskrequirements.ErrInvalidRecordCode, "store is required")
	}
	var out AcceptedGraphVersion
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		existing, ok, err := loadAcceptedGraphVersionInTx(ctx, tx, projectID, deliveryRunID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: accepted graph version is missing", delivery.ErrMissingReference)
		}
		out = existing
		return nil
	})
	return out, err
}

func validateProposalAtAuthorityBoundary(proposal GraphProposal) error {
	if proposal.SchemaVersion != SchemaTaskGraphProposal || proposal.ProjectID == "" || proposal.DeliveryRunID == "" || proposal.IntentSummary == "" {
		return taskErr(taskrequirements.ErrInvalidRecordCode, "task graph proposal has missing required fields")
	}
	if proposal.InputFingerprint == "" || proposal.PolicyFingerprint == "" || proposal.PlanFingerprint == "" || proposal.AuthorizationFingerprint == "" || proposal.PolicyVersion == "" {
		return taskErr(taskrequirements.ErrInvalidRecordCode, "task graph proposal is missing authority fingerprints")
	}
	if proposal.Validation.ValidationStatus != "passed" || proposal.Validation.PlanFingerprint != proposal.PlanFingerprint || proposal.Validation.TaskGraphValidationID == "" {
		return taskErr(taskrequirements.ErrGraphDisconnectedCode, "task graph validation did not pass")
	}
	if proposal.Validation.TaskCount != len(proposal.Tasks) || proposal.Validation.EdgeCount != len(proposal.Edges) {
		return taskErr(taskrequirements.ErrInvalidRecordCode, "task graph validation counts do not match proposal")
	}
	if proposal.ApprovalRequirement != approvalRequirement(proposal.Tasks) {
		return fmt.Errorf("%w: approval requirement does not match task side effects", delivery.ErrStaleApproval)
	}
	if !knownSideEffect(proposal.MaxSideEffectClass) {
		return taskErr(taskrequirements.ErrInvalidRecordCode, "unknown max_side_effect_class %q", proposal.MaxSideEffectClass)
	}
	taskIDs := map[string]string{}
	keyForID := map[string]string{}
	for _, task := range proposal.Tasks {
		if task.Task.ProjectID != proposal.ProjectID || task.Task.DeliveryRunID != proposal.DeliveryRunID {
			return taskErr(taskrequirements.ErrCrossProjectReferenceCode, "task %s belongs to another project or run", task.Task.TaskID)
		}
		if task.Task.TaskID == "" || task.Task.TaskKey == "" || task.Task.Title == "" || !json.Valid([]byte(emptyJSON(task.Task.RequirementsJSON))) || !json.Valid([]byte(emptyJSON(task.Task.ScopeJSON))) {
			return taskErr(taskrequirements.ErrInvalidRecordCode, "task %s has invalid required fields", task.Task.TaskID)
		}
		if task.Task.PlanFingerprint != proposal.PlanFingerprint {
			return fmt.Errorf("%w: task %s plan fingerprint changed", delivery.ErrStaleApproval, task.Task.TaskID)
		}
		if task.TaskRequirementID == "" || task.TaskRequirementFingerprint == "" || task.TaskRequirementPayloadHash == "" {
			return taskErr(taskrequirements.ErrInvalidRecordCode, "task %s is missing requirement identity", task.Task.TaskID)
		}
		if task.Requirement.ProjectID != proposal.ProjectID || task.Requirement.DeliveryRunID != proposal.DeliveryRunID || task.Requirement.TaskID != task.Task.TaskID {
			return taskErr(taskrequirements.ErrCrossProjectReferenceCode, "requirement %s belongs to another task", task.TaskRequirementID)
		}
		if task.Requirement.TaskRequirementID != task.TaskRequirementID || task.Requirement.TaskRequirementFingerprint != task.TaskRequirementFingerprint {
			return fmt.Errorf("%w: requirement identity changed for task %s", delivery.ErrStaleApproval, task.Task.TaskID)
		}
		if string(task.Requirement.SideEffectClass) != task.Task.SideEffectClass || string(task.Requirement.PermissionRequired) != task.Task.Permission {
			return fmt.Errorf("%w: requirement authority changed for task %s", delivery.ErrStaleApproval, task.Task.TaskID)
		}
		reqPayload, err := delivery.CanonicalJSON(task.Requirement)
		if err != nil {
			return taskErr(taskrequirements.ErrInvalidRecordCode, "canonical requirement: %v", err)
		}
		if delivery.SHA256Digest(reqPayload) != task.TaskRequirementPayloadHash {
			return fmt.Errorf("%w: requirement payload changed for task %s", delivery.ErrStaleApproval, task.Task.TaskID)
		}
		if err := taskrequirements.Validate(task.Requirement); err != nil {
			return err
		}
		if sideEffectRank(task.Task.SideEffectClass) > sideEffectRank(proposal.MaxSideEffectClass) {
			return taskErr(taskrequirements.ErrGraphBoundExceededCode, "task %s side effect %s exceeds proposal maximum %s", task.Task.TaskKey, task.Task.SideEffectClass, proposal.MaxSideEffectClass)
		}
		if taskIDs[task.Task.TaskKey] != "" || keyForID[task.Task.TaskID] != "" {
			return taskErr(taskrequirements.ErrDuplicateRecordCode, "duplicate task identity in graph")
		}
		taskIDs[task.Task.TaskKey] = task.Task.TaskID
		keyForID[task.Task.TaskID] = task.Task.TaskKey
	}
	graph, err := graphFromProposal(proposal, taskIDs, keyForID)
	if err != nil {
		return err
	}
	rootKeys, err := keysForTaskIDs(proposal.RootTaskIDs, keyForID, "root_task_ids")
	if err != nil {
		return err
	}
	terminalKeys, err := keysForTaskIDs(proposal.TerminalTaskIDs, keyForID, "terminal_task_ids")
	if err != nil {
		return err
	}
	validation, err := validateGraph(proposal.DeliveryRunID, taskIDs, graph, rootKeys, terminalKeys, proposal.GraphBounds)
	if err != nil {
		return err
	}
	if validation.TaskCount != proposal.Validation.TaskCount ||
		validation.EdgeCount != proposal.Validation.EdgeCount ||
		validation.MaxObservedDepth != proposal.Validation.MaxObservedDepth ||
		validation.MaxObservedFanOut != proposal.Validation.MaxObservedFanOut ||
		validation.MaxObservedDependencies != proposal.Validation.MaxObservedDependencies ||
		!sameStrings(validation.ParallelReadyWidths, proposal.Validation.ParallelReadyWidths) {
		return fmt.Errorf("%w: graph validation metrics changed", delivery.ErrStaleApproval)
	}
	return nil
}

func graphFromProposal(proposal GraphProposal, taskIDs, keyForID map[string]string) (graphState, error) {
	graph := graphState{Edges: make([]delivery.DependencyEdge, 0, len(proposal.Edges)), Outgoing: map[string][]string{}, Incoming: map[string][]string{}, DependsOn: map[string][]string{}, KeyForID: keyForID}
	for _, id := range taskIDs {
		graph.Outgoing[id] = []string{}
		graph.Incoming[id] = []string{}
		graph.DependsOn[id] = []string{}
	}
	seen := map[string]bool{}
	for _, edge := range proposal.Edges {
		if edge.ProjectID != proposal.ProjectID || edge.DeliveryRunID != proposal.DeliveryRunID {
			return graphState{}, taskErr(taskrequirements.ErrCrossProjectReferenceCode, "edge %s belongs to another project or run", edge.EdgeID)
		}
		if edge.PlanFingerprint != proposal.PlanFingerprint {
			return graphState{}, fmt.Errorf("%w: edge %s plan fingerprint changed", delivery.ErrStaleApproval, edge.EdgeID)
		}
		if _, ok := keyForID[edge.FromTaskID]; !ok {
			return graphState{}, taskErr(taskrequirements.ErrMissingReferenceCode, "edge references unknown from_task_id %q", edge.FromTaskID)
		}
		if _, ok := keyForID[edge.ToTaskID]; !ok {
			return graphState{}, taskErr(taskrequirements.ErrMissingReferenceCode, "edge references unknown to_task_id %q", edge.ToTaskID)
		}
		if edge.FromTaskID == edge.ToTaskID {
			return graphState{}, taskErr(taskrequirements.ErrGraphCycleDetectedCode, "self dependency %s", edge.FromTaskID)
		}
		key := edge.FromTaskID + "\x00" + edge.ToTaskID + "\x00" + edge.EdgeKind
		if seen[key] {
			return graphState{}, taskErr(taskrequirements.ErrDuplicateRecordCode, "duplicate edge %s", edge.EdgeID)
		}
		seen[key] = true
		graph.Outgoing[edge.FromTaskID] = append(graph.Outgoing[edge.FromTaskID], edge.ToTaskID)
		graph.Incoming[edge.ToTaskID] = append(graph.Incoming[edge.ToTaskID], edge.FromTaskID)
		graph.DependsOn[edge.ToTaskID] = append(graph.DependsOn[edge.ToTaskID], edge.FromTaskID)
		graph.Edges = append(graph.Edges, edge)
	}
	for id := range graph.DependsOn {
		sortStrings(graph.DependsOn[id])
	}
	return graph, nil
}

func keysForTaskIDs(ids []string, keyForID map[string]string, label string) ([]string, error) {
	keys := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		key := keyForID[id]
		if key == "" {
			return nil, taskErr(taskrequirements.ErrMissingReferenceCode, "%s references unknown task_id %q", label, id)
		}
		if !seen[key] {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	sortStrings(keys)
	return keys, nil
}

func persistAcceptedRun(ctx context.Context, tx storage.Tx, proposal GraphProposal, opts AcceptOptions, now time.Time) error {
	actorJSON, err := jsonString(opts.Actor)
	if err != nil {
		return err
	}
	hostJSON, err := jsonString(opts.Host)
	if err != nil {
		return err
	}
	state := delivery.RunApproved
	if proposal.ApprovalRequirement != "required" {
		state = delivery.RunApproved
	}
	_, err = tx.Exec(ctx, `INSERT INTO delivery_runs(
		delivery_run_id, run_id, schema_version, record_version, project_id, root_run_id, state, intent_summary,
		input_fingerprint, policy_fingerprint, plan_fingerprint, authorization_fingerprint, policy_version, max_side_effect_class,
		approval_status, override_status, created_at, updated_at, planned_at, approved_at, created_by_json, updated_by_json, host_json, report_ids_json
	) VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'none', ?, ?, ?, ?, ?, ?, ?, '[]')
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
		updated_at = excluded.updated_at,
		planned_at = COALESCE(delivery_runs.planned_at, excluded.planned_at),
		approved_at = COALESCE(delivery_runs.approved_at, excluded.approved_at),
		updated_by_json = excluded.updated_by_json,
		host_json = excluded.host_json`,
		proposal.DeliveryRunID, proposal.DeliveryRunID, delivery.SchemaDeliveryRun, proposal.ProjectID, proposal.DeliveryRunID, state, proposal.IntentSummary,
		proposal.InputFingerprint, proposal.PolicyFingerprint, proposal.PlanFingerprint, proposal.AuthorizationFingerprint, proposal.PolicyVersion, proposal.MaxSideEffectClass,
		"approved", delivery.CanonicalTimestamp(now), delivery.CanonicalTimestamp(now), delivery.CanonicalTimestamp(now), delivery.CanonicalTimestamp(now), actorJSON, actorJSON, hostJSON)
	if err != nil {
		return fmt.Errorf("persist accepted delivery run: %w", err)
	}
	return nil
}

func persistAcceptedPlanFingerprint(ctx context.Context, tx storage.Tx, proposal GraphProposal, opts AcceptOptions, now time.Time) error {
	actorJSON, err := jsonString(opts.Actor)
	if err != nil {
		return err
	}
	hostJSON, err := jsonString(opts.Host)
	if err != nil {
		return err
	}
	canonicalInputs, err := jsonString(map[string]any{
		"proposal_schema_version": proposal.SchemaVersion,
		"plan_seed_fingerprint":   proposal.PlanSeedFingerprint,
		"approved_scope":          approvedScope(proposal.Tasks),
	})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO delivery_plan_fingerprints(
		fingerprint_id, schema_version, record_version, project_id, delivery_run_id, input_fingerprint, policy_fingerprint,
		plan_fingerprint, authorization_fingerprint, canonicalization_version, algorithm, canonical_inputs_json, created_at, created_by_json, host_json
	) VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, 'sha256', ?, ?, ?, ?)
	ON CONFLICT(fingerprint_id) DO NOTHING`,
		proposal.PlanFingerprint, delivery.SchemaPlanFingerprint, proposal.ProjectID, proposal.DeliveryRunID, proposal.InputFingerprint, proposal.PolicyFingerprint,
		proposal.PlanFingerprint, proposal.AuthorizationFingerprint, delivery.CanonicalJSONVersion, canonicalInputs, delivery.CanonicalTimestamp(now), actorJSON, hostJSON)
	if err != nil {
		return fmt.Errorf("persist accepted plan fingerprint: %w", err)
	}
	return nil
}

func persistAcceptedTask(ctx context.Context, tx storage.Tx, task delivery.Task, auth string) error {
	createdBy, err := jsonString(task.CreatedBy)
	if err != nil {
		return err
	}
	updatedBy, err := jsonString(task.UpdatedBy)
	if err != nil {
		return err
	}
	hostJSON, err := jsonString(task.Host)
	if err != nil {
		return err
	}
	dependsOn, err := jsonString(task.DependsOn)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO delivery_tasks(
		task_id, schema_version, record_version, project_id, delivery_run_id, task_key, state, title, requirements_json, scope_json,
		permission, side_effect_class, policy_version, plan_fingerprint, authorization_fingerprint, attempt_count, depends_on_json,
		created_at, updated_at, created_by_json, updated_by_json, host_json, terminal_error_code, error_message
	) VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?)`,
		task.TaskID, delivery.SchemaTask, task.ProjectID, task.DeliveryRunID, task.TaskKey, task.State, task.Title, emptyJSON(task.RequirementsJSON), emptyJSON(task.ScopeJSON),
		task.Permission, task.SideEffectClass, task.PolicyVersion, task.PlanFingerprint, auth, dependsOn, task.CreatedAt, task.UpdatedAt, createdBy, updatedBy, hostJSON,
		task.TerminalErrorCode, task.ErrorMessage)
	if err != nil {
		return fmt.Errorf("persist accepted task %s: %w", task.TaskID, err)
	}
	return nil
}

func persistAcceptedRequirement(ctx context.Context, tx storage.Tx, req taskrequirements.TaskRequirement) error {
	payload, err := delivery.CanonicalJSON(req)
	if err != nil {
		return taskErr(taskrequirements.ErrInvalidRecordCode, "canonical requirement payload: %v", err)
	}
	createdBy, err := jsonString(req.CreatedBy)
	if err != nil {
		return err
	}
	updatedBy, err := jsonString(req.UpdatedBy)
	if err != nil {
		return err
	}
	hostJSON, err := jsonString(req.Host)
	if err != nil {
		return err
	}
	rules, err := jsonString(req.ClassificationRules)
	if err != nil {
		return err
	}
	caps, err := jsonString(req.RequiredCapabilities)
	if err != nil {
		return err
	}
	verification, err := jsonString(req.VerificationRequirements)
	if err != nil {
		return err
	}
	heuristics, err := jsonString(req.Heuristics)
	if err != nil {
		return err
	}
	source, err := jsonString(req.Source)
	if err != nil {
		return err
	}
	gaps, err := jsonString(req.GapReasons)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO task_requirements(
		task_requirement_id, schema_version, record_version, task_requirement_fingerprint, project_id, delivery_run_id, task_id, task_key,
		role_key, risk_tier, permission_required, side_effect_class, required_output, scope_json, data_classification, network_required,
		nested_allowed, cancellation_required, quality_floor, classification_rules_json, required_capabilities_json, verification_requirements_json,
		heuristics_json, provenance_summary, policy_version, plan_fingerprint, created_at, updated_at, created_by_json, updated_by_json,
		host_json, classification, confidence, source_json, heuristic, heuristic_reason, terminal_error_code, gap_reasons_json, payload_json
	) VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.TaskRequirementID, taskrequirements.SchemaTaskRequirement, req.TaskRequirementFingerprint, req.ProjectID, req.DeliveryRunID, req.TaskID, req.TaskKey,
		req.RoleKey, string(req.RiskTier), string(req.PermissionRequired), string(req.SideEffectClass), string(req.RequiredOutput), req.ScopeJSON, req.DataClassification, string(req.NetworkRequired),
		boolInt(req.NestedAllowed), boolInt(req.CancellationRequired), string(req.QualityFloor), rules, caps, verification, heuristics, req.ProvenanceSummary,
		req.PolicyVersion, req.PlanFingerprint, req.CreatedAt, req.UpdatedAt, createdBy, updatedBy, hostJSON, req.Classification, string(req.Confidence),
		source, boolInt(req.Heuristic), req.HeuristicReason, req.TerminalErrorCode, gaps, string(payload))
	if err != nil {
		return fmt.Errorf("persist accepted task requirement %s: %w", req.TaskRequirementID, err)
	}
	return nil
}

func persistAcceptedEdge(ctx context.Context, tx storage.Tx, edge delivery.DependencyEdge) error {
	createdBy, err := jsonString(edge.CreatedBy)
	if err != nil {
		return err
	}
	hostJSON, err := jsonString(edge.Host)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO delivery_dependency_edges(
		edge_id, schema_version, record_version, project_id, delivery_run_id, from_task_id, to_task_id, edge_kind, ordinal,
		plan_fingerprint, created_at, updated_at, created_by_json, host_json
	) VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		edge.EdgeID, delivery.SchemaDependencyEdge, edge.ProjectID, edge.DeliveryRunID, edge.FromTaskID, edge.ToTaskID, edge.EdgeKind, edge.Ordinal,
		edge.PlanFingerprint, edge.CreatedAt, edge.UpdatedAt, createdBy, hostJSON)
	if err != nil {
		return fmt.Errorf("persist accepted dependency edge %s: %w", edge.EdgeID, err)
	}
	return nil
}

func persistAcceptedApproval(ctx context.Context, tx storage.Tx, proposal GraphProposal, approvalID string, opts AcceptOptions, now time.Time) error {
	actorJSON, err := jsonString(opts.Actor)
	if err != nil {
		return err
	}
	hostJSON, err := jsonString(opts.Host)
	if err != nil {
		return err
	}
	scope, err := jsonString(approvedScope(proposal.Tasks))
	if err != nil {
		return err
	}
	nowText := delivery.CanonicalTimestamp(now)
	_, err = tx.Exec(ctx, `INSERT INTO delivery_approvals(
		approval_id, schema_version, record_version, project_id, delivery_run_id, approval_kind, authorization_fingerprint,
		input_fingerprint, policy_fingerprint, plan_fingerprint, approved_side_effect_class, approved_scope_json,
		approved_by_json, status, approved_at, created_at, updated_at, created_by_json, updated_by_json, host_json
	) VALUES (?, ?, 1, ?, ?, 'plan', ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?, ?, ?, ?, ?)`,
		approvalID, delivery.SchemaApproval, proposal.ProjectID, proposal.DeliveryRunID, proposal.AuthorizationFingerprint,
		proposal.InputFingerprint, proposal.PolicyFingerprint, proposal.PlanFingerprint, proposal.MaxSideEffectClass, scope,
		actorJSON, nowText, nowText, nowText, actorJSON, actorJSON, hostJSON)
	if err != nil {
		return fmt.Errorf("persist accepted approval: %w", err)
	}
	return nil
}

func persistGraphValidation(ctx context.Context, tx storage.Tx, proposal GraphProposal, opts AcceptOptions, canonicalProposal []byte, now time.Time) error {
	validationJSON, err := jsonString(proposal.Validation)
	if err != nil {
		return err
	}
	actorJSON, err := jsonString(opts.Actor)
	if err != nil {
		return err
	}
	hostJSON, err := jsonString(opts.Host)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO task_graph_validations(
		task_graph_validation_id, schema_version, record_version, project_id, delivery_run_id, plan_fingerprint, authorization_fingerprint,
		validation_status, validation_json, created_at, created_by_json, host_json
	) VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		proposal.Validation.TaskGraphValidationID, SchemaTaskGraphValidation, proposal.ProjectID, proposal.DeliveryRunID, proposal.PlanFingerprint,
		proposal.AuthorizationFingerprint, proposal.Validation.ValidationStatus, validationJSON, delivery.CanonicalTimestamp(now), actorJSON, hostJSON)
	if err != nil {
		return fmt.Errorf("persist task graph validation: %w", err)
	}
	_ = canonicalProposal
	return nil
}

func persistGraphVersion(ctx context.Context, tx storage.Tx, version AcceptedGraphVersion, canonicalProposal []byte) error {
	acceptedBy, err := jsonString(version.AcceptedBy)
	if err != nil {
		return err
	}
	hostJSON, err := jsonString(version.Host)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO accepted_task_graph_versions(
		graph_version_id, schema_version, record_version, project_id, delivery_run_id, input_fingerprint, policy_fingerprint,
		plan_fingerprint, authorization_fingerprint, policy_version, routing_policy_profile_id, routing_policy_profile_fingerprint,
		task_graph_validation_id, approval_id, task_count, edge_count, graph_fingerprint, canonical_proposal_hash,
		proposal_json, accepted_at, accepted_by_json, host_json
	) VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		version.GraphVersionID, version.SchemaVersion, version.ProjectID, version.DeliveryRunID, version.InputFingerprint, version.PolicyFingerprint,
		version.PlanFingerprint, version.AuthorizationFingerprint, version.PolicyVersion, version.RoutingPolicyProfileID, version.RoutingPolicyProfileFingerprint,
		version.TaskGraphValidationID, version.ApprovalID, version.TaskCount, version.EdgeCount, version.GraphFingerprint, version.CanonicalProposalHash,
		string(canonicalProposal), version.AcceptedAt, acceptedBy, hostJSON)
	if err != nil {
		return fmt.Errorf("persist accepted task graph version: %w", err)
	}
	return nil
}

func ensureProjectExists(ctx context.Context, tx storage.Tx, projectID string) error {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM projects WHERE id = ?`, projectID).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: project %s is missing", delivery.ErrMissingReference, projectID)
		}
		return fmt.Errorf("inspect project: %w", err)
	}
	return nil
}

func ensureRunAcceptable(ctx context.Context, tx storage.Tx, proposal GraphProposal) error {
	var auth, input, policy, plan string
	err := tx.QueryRow(ctx, `SELECT authorization_fingerprint, input_fingerprint, policy_fingerprint, plan_fingerprint FROM delivery_runs WHERE project_id = ? AND delivery_run_id = ?`, proposal.ProjectID, proposal.DeliveryRunID).Scan(&auth, &input, &policy, &plan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("inspect existing delivery run: %w", err)
	}
	if auth != "" && auth != proposal.AuthorizationFingerprint {
		return fmt.Errorf("%w: existing delivery run authorization fingerprint differs", delivery.ErrStaleApproval)
	}
	if input != "" && input != proposal.InputFingerprint {
		return fmt.Errorf("%w: existing delivery run input fingerprint differs", delivery.ErrStaleApproval)
	}
	if policy != "" && policy != proposal.PolicyFingerprint {
		return fmt.Errorf("%w: existing delivery run policy fingerprint differs", delivery.ErrStaleApproval)
	}
	if plan != "" && plan != proposal.PlanFingerprint {
		return fmt.Errorf("%w: existing delivery run plan fingerprint differs", delivery.ErrStaleApproval)
	}
	var residual int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM delivery_tasks WHERE project_id = ? AND delivery_run_id = ?`, proposal.ProjectID, proposal.DeliveryRunID).Scan(&residual); err != nil {
		return fmt.Errorf("inspect existing delivery tasks: %w", err)
	}
	if residual > 0 {
		return taskErr(taskrequirements.ErrDuplicateRecordCode, "delivery run already has task residual state")
	}
	return nil
}

func loadAcceptedGraphVersionInTx(ctx context.Context, tx storage.Tx, projectID, deliveryRunID string) (AcceptedGraphVersion, bool, error) {
	var out AcceptedGraphVersion
	var proposalJSON, acceptedByJSON, hostJSON string
	err := tx.QueryRow(ctx, `SELECT graph_version_id, schema_version, record_version, project_id, delivery_run_id, input_fingerprint, policy_fingerprint,
		plan_fingerprint, authorization_fingerprint, policy_version, routing_policy_profile_id, routing_policy_profile_fingerprint,
		task_graph_validation_id, approval_id, task_count, edge_count, graph_fingerprint, canonical_proposal_hash,
		proposal_json, accepted_at, accepted_by_json, host_json
		FROM accepted_task_graph_versions WHERE project_id = ? AND delivery_run_id = ?`, projectID, deliveryRunID).Scan(
		&out.GraphVersionID, &out.SchemaVersion, &out.RecordVersion, &out.ProjectID, &out.DeliveryRunID, &out.InputFingerprint, &out.PolicyFingerprint,
		&out.PlanFingerprint, &out.AuthorizationFingerprint, &out.PolicyVersion, &out.RoutingPolicyProfileID, &out.RoutingPolicyProfileFingerprint,
		&out.TaskGraphValidationID, &out.ApprovalID, &out.TaskCount, &out.EdgeCount, &out.GraphFingerprint, &out.CanonicalProposalHash,
		&proposalJSON, &out.AcceptedAt, &acceptedByJSON, &hostJSON)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AcceptedGraphVersion{}, false, nil
		}
		return AcceptedGraphVersion{}, false, fmt.Errorf("load accepted task graph version: %w", err)
	}
	if err := json.Unmarshal([]byte(proposalJSON), &out.Proposal); err != nil {
		return AcceptedGraphVersion{}, false, taskErr(taskrequirements.ErrInvalidRecordCode, "stored graph proposal is invalid: %v", err)
	}
	if err := json.Unmarshal([]byte(acceptedByJSON), &out.AcceptedBy); err != nil {
		return AcceptedGraphVersion{}, false, taskErr(taskrequirements.ErrInvalidRecordCode, "stored accepted_by is invalid: %v", err)
	}
	if err := json.Unmarshal([]byte(hostJSON), &out.Host); err != nil {
		return AcceptedGraphVersion{}, false, taskErr(taskrequirements.ErrInvalidRecordCode, "stored host is invalid: %v", err)
	}
	return out, true, nil
}

func replayAcceptedGraph(ctx context.Context, tx storage.Tx, key, projectID, runID string, request any, out *AcceptedGraphVersion) (bool, error) {
	key, err := acceptedGraphIdempotencyKey(key, request)
	if err != nil {
		return false, err
	}
	requestBytes, err := delivery.CanonicalJSON(request)
	if err != nil {
		return false, err
	}
	requestHash := delivery.SHA256Digest(requestBytes)
	var storedHash, resultJSON string
	err = tx.QueryRow(ctx, `SELECT request_hash, result_json FROM delivery_idempotency WHERE idempotency_key = ?`, key).Scan(&storedHash, &resultJSON)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("inspect task graph idempotency key: %w", err)
	}
	if storedHash != requestHash {
		return false, fmt.Errorf("%w: idempotency key %s replayed with different accepted graph request", delivery.ErrDuplicateReplay, key)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(resultJSON), out); err != nil {
			return false, taskErr(taskrequirements.ErrInvalidRecordCode, "idempotency result is invalid")
		}
	}
	_ = projectID
	_ = runID
	return true, nil
}

func rememberAcceptedGraph(ctx context.Context, tx storage.Tx, key, projectID, runID string, request any, result AcceptedGraphVersion, now time.Time) error {
	key, err := acceptedGraphIdempotencyKey(key, request)
	if err != nil {
		return err
	}
	requestBytes, err := delivery.CanonicalJSON(request)
	if err != nil {
		return err
	}
	resultBytes, err := json.Marshal(result)
	if err != nil {
		return taskErr(taskrequirements.ErrInvalidRecordCode, "marshal idempotency result: %v", err)
	}
	_, err = tx.Exec(ctx, `INSERT OR IGNORE INTO delivery_idempotency(idempotency_key, project_id, delivery_run_id, operation, request_hash, request_json, result_json, created_at) VALUES (?, ?, ?, 'accept_task_graph', ?, ?, ?, ?)`,
		key, projectID, runID, delivery.SHA256Digest(requestBytes), string(requestBytes), string(resultBytes), delivery.CanonicalTimestamp(now))
	if err != nil {
		return fmt.Errorf("record task graph idempotency key: %w", err)
	}
	return nil
}

func acceptedGraphIdempotencyKey(key string, request any) (string, error) {
	key = strings.TrimSpace(key)
	if key != "" {
		return key, nil
	}
	bytes, err := delivery.CanonicalJSON(request)
	if err != nil {
		return "", fmt.Errorf("derive task graph idempotency key: %w", err)
	}
	return "idem_" + digestBase32("accept_task_graph", string(bytes))[:32], nil
}

func jsonString(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", taskErr(taskrequirements.ErrInvalidRecordCode, "marshal JSON: %v", err)
	}
	return string(data), nil
}

func emptyJSON(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "{}"
	}
	return value
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func sameStrings(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sortStrings(values []string) {
	if len(values) < 2 {
		return
	}
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
