package delivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/storage"
)

const (
	PlanProposalSchema = "loopcoder.delivery_plan_proposal.v1"

	DecisionActionApprove   = "approve"
	DecisionActionReject    = "reject"
	DecisionActionEdit      = "edit"
	DecisionActionExpire    = "expire"
	DecisionActionSupersede = "supersede"
)

type PlanProposalInput struct {
	ProjectID       string
	DeliveryRunID   string
	HostEnforcement HostEnforcement
}

type PlanProposal struct {
	SchemaVersion            string             `json:"schema_version"`
	ProjectID                string             `json:"project_id"`
	DeliveryRunID            string             `json:"delivery_run_id"`
	RunState                 string             `json:"run_state"`
	IntentSummary            string             `json:"intent_summary"`
	InputFingerprint         string             `json:"input_fingerprint"`
	PolicyFingerprint        string             `json:"policy_fingerprint"`
	PlanFingerprint          string             `json:"plan_fingerprint"`
	AuthorizationFingerprint string             `json:"authorization_fingerprint"`
	StoredAuthorization      string             `json:"stored_authorization_fingerprint,omitempty"`
	FingerprintStatus        string             `json:"fingerprint_status"`
	ApprovalStatus           string             `json:"approval_status"`
	OverrideStatus           string             `json:"override_status"`
	ApprovalRequirement      string             `json:"approval_requirement"`
	PolicyVersion            string             `json:"policy_version"`
	MaxSideEffectClass       string             `json:"max_side_effect_class"`
	ApprovedScopeJSON        string             `json:"approved_scope_json"`
	Invocation               InvocationEvidence `json:"invocation"`
	AuthorizedInvocation     InvocationEvidence `json:"authorized_invocation,omitempty"`
	TaskCount                int                `json:"task_count"`
	EdgeCount                int                `json:"edge_count"`
	Tasks                    []PlanProposalTask `json:"tasks"`
	Edges                    []PlanProposalEdge `json:"edges"`
}

type PlanProposalTask struct {
	TaskID          string `json:"task_id"`
	TaskKey         string `json:"task_key"`
	State           string `json:"state"`
	Title           string `json:"title"`
	Requirements    any    `json:"requirements"`
	Scope           any    `json:"scope"`
	Permission      string `json:"permission"`
	SideEffectClass string `json:"side_effect_class"`
	PolicyVersion   string `json:"policy_version"`
}

type PlanProposalEdge struct {
	EdgeID     string `json:"edge_id"`
	FromTaskID string `json:"from_task_id"`
	ToTaskID   string `json:"to_task_id"`
	EdgeKind   string `json:"edge_kind"`
	Ordinal    int    `json:"ordinal"`
}

type DecisionOptions struct {
	ProjectID                        string
	DeliveryRunID                    string
	Action                           string
	ExpectedAuthorizationFingerprint string
	Actor                            Actor
	Host                             Host
	IdempotencyKey                   string
	Now                              time.Time
	ExpiresAt                        string
	EditedProposalJSON               string
	Reason                           string
	HostEnforcement                  HostEnforcement
}

type DecisionResult struct {
	Action                   string             `json:"action"`
	ProjectID                string             `json:"project_id"`
	DeliveryRunID            string             `json:"delivery_run_id"`
	AuthorizationFingerprint string             `json:"authorization_fingerprint"`
	RunState                 string             `json:"run_state"`
	ApprovalStatus           string             `json:"approval_status"`
	DecisionID               string             `json:"decision_id,omitempty"`
	ApprovalID               string             `json:"approval_id,omitempty"`
	Outcome                  string             `json:"outcome"`
	Invocation               InvocationEvidence `json:"invocation"`
	AuthorizedInvocation     InvocationEvidence `json:"authorized_invocation,omitempty"`
	Proposal                 PlanProposal       `json:"proposal"`
}

type ContinueOptions struct {
	ProjectID                        string
	DeliveryRunID                    string
	ExpectedAuthorizationFingerprint string
	Actor                            Actor
	Host                             Host
	IdempotencyKey                   string
	Now                              time.Time
	HostEnforcement                  HostEnforcement
}

type ContinueResult struct {
	ProjectID                string             `json:"project_id"`
	DeliveryRunID            string             `json:"delivery_run_id"`
	AuthorizationFingerprint string             `json:"authorization_fingerprint"`
	RunState                 string             `json:"run_state"`
	ApprovalStatus           string             `json:"approval_status"`
	Outcome                  string             `json:"outcome"`
	Invocation               InvocationEvidence `json:"invocation"`
	AuthorizedInvocation     InvocationEvidence `json:"authorized_invocation,omitempty"`
	Proposal                 PlanProposal       `json:"proposal"`
}

func Plan(ctx context.Context, store storage.Store, input PlanProposalInput) (PlanProposal, error) {
	if store == nil {
		return PlanProposal{}, typed(ErrInvalidRecordCode, "store is required")
	}
	input.ProjectID = strings.TrimSpace(input.ProjectID)
	input.DeliveryRunID = strings.TrimSpace(input.DeliveryRunID)
	if input.ProjectID == "" || input.DeliveryRunID == "" {
		return PlanProposal{}, typed(ErrInvalidRecordCode, "project_id and delivery_run_id are required")
	}
	invocation, err := enforceHostInvocation(OperationPlan, input.HostEnforcement)
	if err != nil {
		return PlanProposal{Invocation: invocation}, err
	}
	var proposal PlanProposal
	err = store.WithTx(ctx, func(tx storage.Tx) error {
		var err error
		proposal, err = planInTx(ctx, tx, input.ProjectID, input.DeliveryRunID, input.HostEnforcement)
		proposal.Invocation = invocation
		return err
	})
	return proposal, err
}

func Decide(ctx context.Context, store storage.Store, opts DecisionOptions) (DecisionResult, error) {
	if store == nil {
		return DecisionResult{}, typed(ErrInvalidRecordCode, "store is required")
	}
	if opts.Now.IsZero() {
		return DecisionResult{}, typed(ErrInvalidRecordCode, "now is required")
	}
	opts.Action = strings.ToLower(strings.TrimSpace(opts.Action))
	if strings.TrimSpace(opts.ExpectedAuthorizationFingerprint) == "" {
		return DecisionResult{}, typed(ErrInvalidRecordCode, "expected authorization fingerprint is required for decisions")
	}
	if !validDecisionAction(opts.Action) {
		return DecisionResult{}, typed(ErrInvalidRecordCode, "unknown decision action %q", opts.Action)
	}
	invocation, err := enforceHostInvocation(OperationDecide, opts.HostEnforcement)
	if err != nil {
		return DecisionResult{Action: opts.Action, ProjectID: opts.ProjectID, DeliveryRunID: opts.DeliveryRunID, Outcome: OutcomeUnsupported, Invocation: invocation}, err
	}
	if ctx.Err() != nil {
		_ = recordInterruptedOutcome(store, opts.ProjectID, opts.DeliveryRunID, OperationDecide, opts.Actor, opts.Host, opts.Now)
		return DecisionResult{Action: opts.Action, ProjectID: opts.ProjectID, DeliveryRunID: opts.DeliveryRunID, Outcome: OutcomeInterrupted, Invocation: invocation}, typed(ErrInvocationInterruptedCode, "%s interrupted before mutation", OperationDecide)
	}
	var out DecisionResult
	err = withWrite(ctx, store, func(tx storage.Tx) error {
		request := map[string]any{
			"operation":                          "delivery_decide",
			"project_id":                         opts.ProjectID,
			"delivery_run_id":                    opts.DeliveryRunID,
			"action":                             opts.Action,
			"expected_authorization_fingerprint": opts.ExpectedAuthorizationFingerprint,
			"expires_at":                         opts.ExpiresAt,
			"edited_proposal_json":               opts.EditedProposalJSON,
			"reason":                             opts.Reason,
			"actor":                              opts.Actor,
			"invocation":                         invocation,
		}
		replayed, err := replay(ctx, tx, opts.IdempotencyKey, opts.ProjectID, opts.DeliveryRunID, "delivery_decide", request, &out)
		if err != nil || replayed {
			return err
		}
		proposal, err := planInTx(ctx, tx, opts.ProjectID, opts.DeliveryRunID, opts.HostEnforcement)
		if err != nil {
			return err
		}
		if err := requireExpectedFingerprint(opts.ExpectedAuthorizationFingerprint, proposal.AuthorizationFingerprint); err != nil {
			return err
		}
		if err := validatePlanPolicy(proposal); err != nil {
			return err
		}
		state, approvalStatus, err := applyDecisionInTx(ctx, tx, proposal, opts)
		if err != nil {
			return err
		}
		out = DecisionResult{
			Action:                   opts.Action,
			ProjectID:                proposal.ProjectID,
			DeliveryRunID:            proposal.DeliveryRunID,
			AuthorizationFingerprint: proposal.AuthorizationFingerprint,
			RunState:                 state,
			ApprovalStatus:           approvalStatus,
			Outcome:                  decisionOutcome(opts.Action),
			Invocation:               invocation,
			AuthorizedInvocation:     proposal.AuthorizedInvocation,
			Proposal:                 proposal,
		}
		decisionID, err := recordDecisionInTx(ctx, tx, proposal, opts)
		if err != nil {
			return err
		}
		out.DecisionID = decisionID
		if opts.Action == DecisionActionApprove {
			out.ApprovalID = stableApprovalID(proposal, opts.Actor)
		}
		return remember(ctx, tx, opts.IdempotencyKey, opts.ProjectID, opts.DeliveryRunID, "delivery_decide", request, out, opts.Now)
	})
	return out, err
}

func Continue(ctx context.Context, store storage.Store, opts ContinueOptions) (ContinueResult, error) {
	if store == nil {
		return ContinueResult{}, typed(ErrInvalidRecordCode, "store is required")
	}
	if opts.Now.IsZero() {
		return ContinueResult{}, typed(ErrInvalidRecordCode, "now is required")
	}
	invocation, err := enforceHostInvocation(OperationContinue, opts.HostEnforcement)
	if err != nil {
		return ContinueResult{ProjectID: opts.ProjectID, DeliveryRunID: opts.DeliveryRunID, Outcome: OutcomeUnsupported, Invocation: invocation}, err
	}
	if ctx.Err() != nil {
		_ = recordInterruptedOutcome(store, opts.ProjectID, opts.DeliveryRunID, OperationContinue, opts.Actor, opts.Host, opts.Now)
		return ContinueResult{ProjectID: opts.ProjectID, DeliveryRunID: opts.DeliveryRunID, Outcome: OutcomeInterrupted, Invocation: invocation}, typed(ErrInvocationInterruptedCode, "%s interrupted before mutation", OperationContinue)
	}
	var out ContinueResult
	err = withWrite(ctx, store, func(tx storage.Tx) error {
		request := map[string]any{
			"operation":                          "delivery_continue",
			"project_id":                         opts.ProjectID,
			"delivery_run_id":                    opts.DeliveryRunID,
			"expected_authorization_fingerprint": opts.ExpectedAuthorizationFingerprint,
			"actor":                              opts.Actor,
			"invocation":                         invocation,
		}
		replayed, err := replay(ctx, tx, opts.IdempotencyKey, opts.ProjectID, opts.DeliveryRunID, "delivery_continue", request, &out)
		if err != nil || replayed {
			return err
		}
		proposal, err := planInTx(ctx, tx, opts.ProjectID, opts.DeliveryRunID, opts.HostEnforcement)
		if err != nil {
			return err
		}
		if err := requireExpectedFingerprint(opts.ExpectedAuthorizationFingerprint, proposal.AuthorizationFingerprint); err != nil {
			return err
		}
		if proposal.FingerprintStatus == "stale" {
			if err := markRunApprovalStatus(ctx, tx, proposal, "stale", opts.Actor, opts.Host, opts.Now); err != nil {
				return err
			}
			return ErrStaleApproval
		}
		if proposal.ApprovalStatus == "rejected" {
			return typed(ErrPolicyDeniedCode, "delivery run %s was rejected", proposal.DeliveryRunID)
		}
		if err := validatePlanPolicy(proposal); err != nil {
			return err
		}
		if err := ensureActiveApprovalForContinue(ctx, tx, proposal, opts.Now); err != nil {
			if errors.Is(err, ErrExpiredApproval) {
				_ = markRunApprovalStatus(ctx, tx, proposal, "expired", opts.Actor, opts.Host, opts.Now)
			}
			return err
		}
		state, err := advanceRunForContinue(ctx, tx, proposal, opts.Actor, opts.Host, opts.Now)
		if err != nil {
			return err
		}
		if err := readyApprovedTasks(ctx, tx, proposal, opts.Actor, opts.Host, opts.Now); err != nil {
			return err
		}
		out = ContinueResult{
			ProjectID:                proposal.ProjectID,
			DeliveryRunID:            proposal.DeliveryRunID,
			AuthorizationFingerprint: proposal.AuthorizationFingerprint,
			RunState:                 state,
			ApprovalStatus:           "approved",
			Outcome:                  OutcomeAccepted,
			Invocation:               invocation,
			AuthorizedInvocation:     proposal.AuthorizedInvocation,
			Proposal:                 proposal,
		}
		return remember(ctx, tx, opts.IdempotencyKey, opts.ProjectID, opts.DeliveryRunID, "delivery_continue", request, out, opts.Now)
	})
	return out, err
}

func planInTx(ctx context.Context, tx storage.Tx, projectID, deliveryRunID string, enforcement HostEnforcement) (PlanProposal, error) {
	run, err := loadPlanRun(ctx, tx, projectID, deliveryRunID)
	if err != nil {
		return PlanProposal{}, err
	}
	tasks, err := loadPlanTasks(ctx, tx, projectID, deliveryRunID)
	if err != nil {
		return PlanProposal{}, err
	}
	edges, err := loadPlanEdges(ctx, tx, projectID, deliveryRunID)
	if err != nil {
		return PlanProposal{}, err
	}
	scope, err := approvedScope(tasks)
	if err != nil {
		return PlanProposal{}, err
	}
	authorizedInvocation, err := authorizationInvocationEvidence(enforcement)
	if err != nil {
		return PlanProposal{}, err
	}
	inputPayload := map[string]any{
		"schema_version":  "loopcoder.delivery_input.v1",
		"project_id":      run.ProjectID,
		"delivery_run_id": run.DeliveryRunID,
		"intent_summary":  run.IntentSummary,
	}
	if enforcement.Provided {
		inputPayload["authorized_invocation"] = authorizedInvocation
	}
	inputFingerprint, _, err := DigestCanonicalJSON(inputPayload)
	if err != nil {
		return PlanProposal{}, err
	}
	policyFingerprint, _, err := DigestCanonicalJSON(map[string]any{
		"schema_version":         "loopcoder.delivery_policy.v1",
		"policy_version":         run.PolicyVersion,
		"max_side_effect_class":  run.MaxSideEffectClass,
		"approval_required_when": "side_effect_class_above_none",
	})
	if err != nil {
		return PlanProposal{}, err
	}
	planFingerprint, _, err := DigestCanonicalJSON(map[string]any{
		"schema_version": "loopcoder.delivery_plan.v1",
		"tasks":          proposalTasksForFingerprint(tasks),
		"edges":          proposalEdgesForFingerprint(edges),
	})
	if err != nil {
		return PlanProposal{}, err
	}
	auth, _, err := AuthorizationFingerprint(inputFingerprint, policyFingerprint, planFingerprint, run.MaxSideEffectClass, scope)
	if err != nil {
		return PlanProposal{}, err
	}
	scopeJSON, err := CanonicalJSON(scope)
	if err != nil {
		return PlanProposal{}, err
	}
	fingerprintStatus := "current"
	if strings.TrimSpace(run.AuthorizationFingerprint) != "" && run.AuthorizationFingerprint != auth {
		fingerprintStatus = "stale"
	}
	return PlanProposal{
		SchemaVersion:            PlanProposalSchema,
		ProjectID:                run.ProjectID,
		DeliveryRunID:            run.DeliveryRunID,
		RunState:                 run.State,
		IntentSummary:            run.IntentSummary,
		InputFingerprint:         inputFingerprint,
		PolicyFingerprint:        policyFingerprint,
		PlanFingerprint:          planFingerprint,
		AuthorizationFingerprint: auth,
		StoredAuthorization:      run.AuthorizationFingerprint,
		FingerprintStatus:        fingerprintStatus,
		ApprovalStatus:           run.ApprovalStatus,
		OverrideStatus:           run.OverrideStatus,
		ApprovalRequirement:      approvalRequirement(tasks),
		PolicyVersion:            run.PolicyVersion,
		MaxSideEffectClass:       run.MaxSideEffectClass,
		ApprovedScopeJSON:        string(scopeJSON),
		AuthorizedInvocation:     authorizedInvocation,
		TaskCount:                len(tasks),
		EdgeCount:                len(edges),
		Tasks:                    tasks,
		Edges:                    edges,
	}, nil
}

func applyDecisionInTx(ctx context.Context, tx storage.Tx, proposal PlanProposal, opts DecisionOptions) (string, string, error) {
	switch opts.Action {
	case DecisionActionApprove:
		if proposal.ApprovalStatus == "rejected" {
			return "", "", typed(ErrInvalidTransitionCode, "rejected run cannot be approved without a new plan")
		}
		if err := persistProposalAuthority(ctx, tx, proposal, opts.Actor, opts.Host, opts.Now); err != nil {
			return "", "", err
		}
		if err := insertApprovalInTx(ctx, tx, proposal, opts); err != nil {
			return "", "", err
		}
		state, err := transitionRunAfterDecision(ctx, tx, proposal, "approve", "approved", opts.Actor, opts.Host, opts.Now)
		return state, "approved", err
	case DecisionActionReject:
		if proposal.ApprovalStatus == "approved" {
			return "", "", typed(ErrInvalidTransitionCode, "approved run cannot be rejected by replayed decision")
		}
		state, err := transitionRunAfterDecision(ctx, tx, proposal, "reject", "rejected", opts.Actor, opts.Host, opts.Now)
		return state, "rejected", err
	case DecisionActionEdit:
		state, err := transitionRunAfterDecision(ctx, tx, proposal, "approve_stale", "stale", opts.Actor, opts.Host, opts.Now)
		if errors.Is(err, ErrStaleApproval) {
			return proposal.RunState, "stale", markRunApprovalStatus(ctx, tx, proposal, "stale", opts.Actor, opts.Host, opts.Now)
		}
		return state, "stale", err
	case DecisionActionExpire:
		if err := updateApprovalRows(ctx, tx, proposal, "expired", opts.Now); err != nil {
			return "", "", err
		}
		return proposal.RunState, "expired", markRunApprovalStatus(ctx, tx, proposal, "expired", opts.Actor, opts.Host, opts.Now)
	case DecisionActionSupersede:
		if err := updateApprovalRows(ctx, tx, proposal, "stale", opts.Now); err != nil {
			return "", "", err
		}
		return proposal.RunState, "stale", markRunApprovalStatus(ctx, tx, proposal, "stale", opts.Actor, opts.Host, opts.Now)
	default:
		return "", "", typed(ErrInvalidRecordCode, "unknown decision action %q", opts.Action)
	}
}

func persistProposalAuthority(ctx context.Context, tx storage.Tx, proposal PlanProposal, actor Actor, host Host, now time.Time) error {
	createdBy, err := marshalJSON("plan proposal actor", actor)
	if err != nil {
		return err
	}
	hostJSON, err := marshalJSON("plan proposal host", host)
	if err != nil {
		return err
	}
	scope, err := decodeJSONAny(proposal.ApprovedScopeJSON)
	if err != nil {
		return err
	}
	canonicalInputs, err := CanonicalJSON(map[string]any{
		"approved_scope":        scope,
		"authorized_invocation": proposal.AuthorizedInvocation,
	})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT OR IGNORE INTO delivery_plan_fingerprints(
			fingerprint_id, schema_version, record_version, project_id, delivery_run_id, input_fingerprint, policy_fingerprint,
			plan_fingerprint, authorization_fingerprint, canonicalization_version, algorithm, canonical_inputs_json, created_at, created_by_json, host_json
		) VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, 'sha256', ?, ?, ?, ?)`,
		proposal.AuthorizationFingerprint, SchemaPlanFingerprint, proposal.ProjectID, proposal.DeliveryRunID, proposal.InputFingerprint, proposal.PolicyFingerprint,
		proposal.PlanFingerprint, proposal.AuthorizationFingerprint, CanonicalJSONVersion, string(canonicalInputs), CanonicalTimestamp(now), createdBy, hostJSON); err != nil {
		return fmt.Errorf("persist proposal authority: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE delivery_runs
		SET input_fingerprint = ?, policy_fingerprint = ?, plan_fingerprint = ?, authorization_fingerprint = ?,
			record_version = record_version + 1, updated_at = ?, updated_by_json = ?, host_json = ?, planned_at = COALESCE(planned_at, ?)
		WHERE project_id = ? AND delivery_run_id = ?`,
		proposal.InputFingerprint, proposal.PolicyFingerprint, proposal.PlanFingerprint, proposal.AuthorizationFingerprint,
		CanonicalTimestamp(now), createdBy, hostJSON, CanonicalTimestamp(now), proposal.ProjectID, proposal.DeliveryRunID); err != nil {
		return fmt.Errorf("bind proposal authority: %w", err)
	}
	return nil
}

func insertApprovalInTx(ctx context.Context, tx storage.Tx, proposal PlanProposal, opts DecisionOptions) error {
	approvedBy, err := marshalJSON("approval approved_by", opts.Actor)
	if err != nil {
		return err
	}
	createdBy, err := marshalJSON("approval created_by", opts.Actor)
	if err != nil {
		return err
	}
	hostJSON, err := marshalJSON("approval host", opts.Host)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT OR IGNORE INTO delivery_approvals(
			approval_id, schema_version, record_version, project_id, delivery_run_id, approval_kind, authorization_fingerprint,
			input_fingerprint, policy_fingerprint, plan_fingerprint, approved_side_effect_class, approved_scope_json,
			approved_by_json, status, approved_at, expires_at, created_at, updated_at, created_by_json, updated_by_json, host_json
		) VALUES (?, ?, 1, ?, ?, 'continue', ?, ?, ?, ?, ?, ?, ?, 'active', ?, NULLIF(?, ''), ?, ?, ?, ?, ?)`,
		stableApprovalID(proposal, opts.Actor), SchemaApproval, proposal.ProjectID, proposal.DeliveryRunID, proposal.AuthorizationFingerprint,
		proposal.InputFingerprint, proposal.PolicyFingerprint, proposal.PlanFingerprint, proposal.MaxSideEffectClass, emptyJSON(proposal.ApprovedScopeJSON),
		approvedBy, CanonicalTimestamp(opts.Now), opts.ExpiresAt, CanonicalTimestamp(opts.Now), CanonicalTimestamp(opts.Now), createdBy, createdBy, hostJSON); err != nil {
		return fmt.Errorf("insert approval: %w", err)
	}
	return nil
}

func recordDecisionInTx(ctx context.Context, tx storage.Tx, proposal PlanProposal, opts DecisionOptions) (string, error) {
	output := map[string]any{
		"action":                    opts.Action,
		"authorization_fingerprint": proposal.AuthorizationFingerprint,
		"reason":                    strings.TrimSpace(opts.Reason),
	}
	if strings.TrimSpace(opts.EditedProposalJSON) != "" {
		var edited any
		if err := json.Unmarshal([]byte(opts.EditedProposalJSON), &edited); err != nil {
			return "", typed(ErrInvalidRecordCode, "edited proposal JSON is invalid")
		}
		output["edited_proposal"] = edited
	}
	outputJSON, err := CanonicalJSON(output)
	if err != nil {
		return "", err
	}
	decidedBy, err := marshalJSON("decision actor", opts.Actor)
	if err != nil {
		return "", err
	}
	hostJSON, err := marshalJSON("decision host", opts.Host)
	if err != nil {
		return "", err
	}
	key := decisionKey(opts.Action, proposal.AuthorizationFingerprint)
	id := stableID("dec", proposal.ProjectID, proposal.DeliveryRunID, key)
	if _, err := tx.Exec(ctx, `INSERT OR IGNORE INTO delivery_decisions(
			decision_id, schema_version, record_version, project_id, delivery_run_id, decision_key, decision_kind,
			decided_by_json, inputs_fingerprint, output_json, alternatives_json, heuristic, policy_version,
			side_effect_class, created_at, created_by_json, host_json
		) VALUES (?, ?, 1, ?, ?, ?, 'approval-request', ?, ?, ?, 'null', 0, ?, 'none', ?, ?, ?)`,
		id, SchemaDecision, proposal.ProjectID, proposal.DeliveryRunID, key, decidedBy, proposal.AuthorizationFingerprint,
		string(outputJSON), proposal.PolicyVersion, CanonicalTimestamp(opts.Now), decidedBy, hostJSON); err != nil {
		return "", fmt.Errorf("record delivery decision: %w", err)
	}
	return id, nil
}

func transitionRunAfterDecision(ctx context.Context, tx storage.Tx, proposal PlanProposal, event, approvalStatus string, actor Actor, host Host, now time.Time) (string, error) {
	next, err := DeliveryRunTransition(proposal.RunState, event)
	if err != nil {
		return "", err
	}
	actorJSON, err := marshalJSON("run decision actor", actor)
	if err != nil {
		return "", err
	}
	hostJSON, err := marshalJSON("run decision host", host)
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE delivery_runs
		SET state = ?, approval_status = ?, record_version = record_version + 1, updated_at = ?, updated_by_json = ?, host_json = ?,
			approved_at = CASE WHEN ? = 'approved' THEN COALESCE(approved_at, ?) ELSE approved_at END,
			ended_at = CASE WHEN ? IN ('abandoned', 'failed', 'cancelled', 'needs-human') THEN COALESCE(ended_at, ?) ELSE ended_at END
		WHERE project_id = ? AND delivery_run_id = ?`,
		next, approvalStatus, CanonicalTimestamp(now), actorJSON, hostJSON, approvalStatus, CanonicalTimestamp(now), next, CanonicalTimestamp(now), proposal.ProjectID, proposal.DeliveryRunID); err != nil {
		return "", fmt.Errorf("transition delivery decision: %w", err)
	}
	return next, nil
}

func advanceRunForContinue(ctx context.Context, tx storage.Tx, proposal PlanProposal, actor Actor, host Host, now time.Time) (string, error) {
	state := proposal.RunState
	var err error
	if state == RunAwaitingApproval {
		proposal.RunState = state
		state, err = transitionRunAfterDecision(ctx, tx, proposal, "approve", "approved", actor, host, now)
		if err != nil {
			return "", err
		}
	}
	switch state {
	case RunApproved:
		next, err := DeliveryRunTransition(state, "queue")
		if err != nil {
			return "", err
		}
		actorJSON, err := marshalJSON("run continue actor", actor)
		if err != nil {
			return "", err
		}
		hostJSON, err := marshalJSON("run continue host", host)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `UPDATE delivery_runs SET state = ?, approval_status = 'approved', record_version = record_version + 1, updated_at = ?, updated_by_json = ?, host_json = ? WHERE project_id = ? AND delivery_run_id = ?`,
			next, CanonicalTimestamp(now), actorJSON, hostJSON, proposal.ProjectID, proposal.DeliveryRunID); err != nil {
			return "", fmt.Errorf("continue delivery run: %w", err)
		}
		return next, nil
	case RunQueued, RunRunning:
		return state, markRunApprovalStatus(ctx, tx, proposal, "approved", actor, host, now)
	default:
		return "", typed(ErrInvalidTransitionCode, "delivery run %s is %s", proposal.DeliveryRunID, state)
	}
}

func readyApprovedTasks(ctx context.Context, tx storage.Tx, proposal PlanProposal, actor Actor, host Host, now time.Time) error {
	actorJSON, err := marshalJSON("task approval actor", actor)
	if err != nil {
		return err
	}
	hostJSON, err := marshalJSON("task approval host", host)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE delivery_tasks
		SET state = 'ready', updated_at = ?, updated_by_json = ?, host_json = ?
		WHERE project_id = ? AND delivery_run_id = ? AND state = 'awaiting-approval'`,
		CanonicalTimestamp(now), actorJSON, hostJSON, proposal.ProjectID, proposal.DeliveryRunID)
	if err != nil {
		return fmt.Errorf("ready approved tasks: %w", err)
	}
	return nil
}

func markRunApprovalStatus(ctx context.Context, tx storage.Tx, proposal PlanProposal, status string, actor Actor, host Host, now time.Time) error {
	actorJSON, err := marshalJSON("run approval actor", actor)
	if err != nil {
		return err
	}
	hostJSON, err := marshalJSON("run approval host", host)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE delivery_runs SET approval_status = ?, record_version = record_version + 1, updated_at = ?, updated_by_json = ?, host_json = ? WHERE project_id = ? AND delivery_run_id = ?`,
		status, CanonicalTimestamp(now), actorJSON, hostJSON, proposal.ProjectID, proposal.DeliveryRunID)
	if err != nil {
		return fmt.Errorf("mark run approval status: %w", err)
	}
	return nil
}

func updateApprovalRows(ctx context.Context, tx storage.Tx, proposal PlanProposal, status string, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE delivery_approvals SET status = ?, updated_at = ? WHERE project_id = ? AND delivery_run_id = ? AND authorization_fingerprint = ? AND status = 'active'`,
		status, CanonicalTimestamp(now), proposal.ProjectID, proposal.DeliveryRunID, proposal.AuthorizationFingerprint)
	if err != nil {
		return fmt.Errorf("update approval rows: %w", err)
	}
	return nil
}

func ensureActiveApprovalForContinue(ctx context.Context, tx storage.Tx, proposal PlanProposal, now time.Time) error {
	var expires string
	err := tx.QueryRow(ctx, `SELECT COALESCE(expires_at, '')
		FROM delivery_approvals
		WHERE project_id = ? AND delivery_run_id = ? AND authorization_fingerprint = ? AND status = 'active'
		ORDER BY approved_at DESC LIMIT 1`, proposal.ProjectID, proposal.DeliveryRunID, proposal.AuthorizationFingerprint).Scan(&expires)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return typed(ErrApprovalRequiredCode, "approval for %s is required", proposal.AuthorizationFingerprint)
		}
		return fmt.Errorf("inspect continue approval: %w", err)
	}
	if strings.TrimSpace(expires) == "" {
		return nil
	}
	expiry, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil {
		return typed(ErrInvalidRecordCode, "approval expiry is invalid")
	}
	if !expiry.After(now) {
		return typed(ErrExpiredApprovalCode, "approval for %s expired at %s", proposal.AuthorizationFingerprint, expires)
	}
	return nil
}

func loadPlanRun(ctx context.Context, tx storage.Tx, projectID, deliveryRunID string) (DeliveryRun, error) {
	var run DeliveryRun
	err := tx.QueryRow(ctx, `SELECT project_id, delivery_run_id, state, intent_summary, input_fingerprint, policy_fingerprint, plan_fingerprint, authorization_fingerprint, policy_version, max_side_effect_class, approval_status, override_status
		FROM delivery_runs WHERE project_id = ? AND delivery_run_id = ?`, projectID, deliveryRunID).Scan(
		&run.ProjectID, &run.DeliveryRunID, &run.State, &run.IntentSummary, &run.InputFingerprint, &run.PolicyFingerprint, &run.PlanFingerprint, &run.AuthorizationFingerprint,
		&run.PolicyVersion, &run.MaxSideEffectClass, &run.ApprovalStatus, &run.OverrideStatus)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeliveryRun{}, typed(ErrMissingReferenceCode, "delivery run %s is missing", deliveryRunID)
		}
		return DeliveryRun{}, fmt.Errorf("load delivery run plan: %w", err)
	}
	return run, nil
}

func loadPlanTasks(ctx context.Context, tx storage.Tx, projectID, deliveryRunID string) ([]PlanProposalTask, error) {
	rows, err := tx.Query(ctx, `SELECT task_id, task_key, state, title, requirements_json, scope_json, permission, side_effect_class, policy_version
		FROM delivery_tasks WHERE project_id = ? AND delivery_run_id = ? ORDER BY task_key, task_id`, projectID, deliveryRunID)
	if err != nil {
		return nil, fmt.Errorf("load delivery plan tasks: %w", err)
	}
	defer rows.Close()
	var tasks []PlanProposalTask
	for rows.Next() {
		var task PlanProposalTask
		var requirements, scope string
		if err := rows.Scan(&task.TaskID, &task.TaskKey, &task.State, &task.Title, &requirements, &scope, &task.Permission, &task.SideEffectClass, &task.PolicyVersion); err != nil {
			return nil, fmt.Errorf("load delivery plan task row: %w", err)
		}
		var err error
		task.Requirements, err = decodeJSONAny(requirements)
		if err != nil {
			return nil, err
		}
		task.Scope, err = decodeJSONAny(scope)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load delivery plan task rows: %w", err)
	}
	return tasks, nil
}

func loadPlanEdges(ctx context.Context, tx storage.Tx, projectID, deliveryRunID string) ([]PlanProposalEdge, error) {
	rows, err := tx.Query(ctx, `SELECT edge_id, from_task_id, to_task_id, edge_kind, ordinal
		FROM delivery_dependency_edges WHERE project_id = ? AND delivery_run_id = ? ORDER BY ordinal, edge_id`, projectID, deliveryRunID)
	if err != nil {
		return nil, fmt.Errorf("load delivery plan edges: %w", err)
	}
	defer rows.Close()
	var edges []PlanProposalEdge
	for rows.Next() {
		var edge PlanProposalEdge
		if err := rows.Scan(&edge.EdgeID, &edge.FromTaskID, &edge.ToTaskID, &edge.EdgeKind, &edge.Ordinal); err != nil {
			return nil, fmt.Errorf("load delivery plan edge row: %w", err)
		}
		edges = append(edges, edge)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load delivery plan edge rows: %w", err)
	}
	return edges, nil
}

func decodeJSONAny(value string) (any, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "{}"
	}
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.UseNumber()
	var out any
	if err := decoder.Decode(&out); err != nil {
		return nil, typed(ErrInvalidRecordCode, "decode persisted JSON: %v", err)
	}
	return out, nil
}

func approvedScope(tasks []PlanProposalTask) (any, error) {
	scopes := make([]any, 0, len(tasks))
	for _, task := range tasks {
		scopes = append(scopes, map[string]any{
			"task_key": task.TaskKey,
			"scope":    task.Scope,
		})
	}
	return map[string]any{"tasks": scopes}, nil
}

func proposalTasksForFingerprint(tasks []PlanProposalTask) []any {
	out := make([]any, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, map[string]any{
			"task_key":          task.TaskKey,
			"title":             task.Title,
			"requirements":      task.Requirements,
			"scope":             task.Scope,
			"permission":        task.Permission,
			"side_effect_class": task.SideEffectClass,
			"policy_version":    task.PolicyVersion,
		})
	}
	return out
}

func proposalEdgesForFingerprint(edges []PlanProposalEdge) []any {
	out := make([]any, 0, len(edges))
	for _, edge := range edges {
		out = append(out, map[string]any{
			"from_task_id": edge.FromTaskID,
			"to_task_id":   edge.ToTaskID,
			"edge_kind":    edge.EdgeKind,
			"ordinal":      edge.Ordinal,
		})
	}
	return out
}

func approvalRequirement(tasks []PlanProposalTask) string {
	for _, task := range tasks {
		if sideEffectRank(task.SideEffectClass) > sideEffectRank("none") {
			return "required"
		}
	}
	return "not-required"
}

func validatePlanPolicy(proposal PlanProposal) error {
	for _, task := range proposal.Tasks {
		if sideEffectRank(task.SideEffectClass) > sideEffectRank(proposal.MaxSideEffectClass) {
			return typed(ErrPolicyDeniedCode, "task %s side effect %s exceeds run maximum %s", task.TaskID, task.SideEffectClass, proposal.MaxSideEffectClass)
		}
	}
	return nil
}

func requireExpectedFingerprint(expected, current string) error {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return nil
	}
	if expected != current {
		return typed(ErrStaleApprovalCode, "expected authorization fingerprint does not match current plan")
	}
	return nil
}

func validDecisionAction(action string) bool {
	switch action {
	case DecisionActionApprove, DecisionActionReject, DecisionActionEdit, DecisionActionExpire, DecisionActionSupersede:
		return true
	default:
		return false
	}
}

func stableApprovalID(proposal PlanProposal, actor Actor) string {
	return stableID("appr", proposal.ProjectID, proposal.DeliveryRunID, proposal.AuthorizationFingerprint, actor.ActorID)
}

func decisionKey(action, authorizationFingerprint string) string {
	return "delivery." + action + "." + authorizationFingerprint
}

func decisionOutcome(action string) string {
	switch action {
	case DecisionActionReject:
		return OutcomeDeclined
	case DecisionActionEdit, DecisionActionExpire, DecisionActionSupersede:
		return OutcomeStale
	default:
		return OutcomeAccepted
	}
}

func recordInterruptedOutcome(store storage.Store, projectID, deliveryRunID, operation string, actor Actor, host Host, now time.Time) error {
	if store == nil || now.IsZero() {
		return nil
	}
	return withWrite(context.Background(), store, func(tx storage.Tx) error {
		proposal, err := planInTx(context.Background(), tx, projectID, deliveryRunID, HostEnforcement{})
		if err != nil {
			return err
		}
		if !RunTerminal(proposal.RunState) {
			next, transitionErr := DeliveryRunTransition(proposal.RunState, "pause")
			if transitionErr == nil {
				actorJSON, err := marshalJSON("interrupted actor", actor)
				if err != nil {
					return err
				}
				hostJSON, err := marshalJSON("interrupted host", host)
				if err != nil {
					return err
				}
				if _, err := tx.Exec(context.Background(), `UPDATE delivery_runs
					SET state = ?, error_message = ?, record_version = record_version + 1,
						updated_at = ?, updated_by_json = ?, host_json = ?
					WHERE project_id = ? AND delivery_run_id = ?`,
					next, operation+" interrupted before completion", CanonicalTimestamp(now),
					actorJSON, hostJSON, projectID, deliveryRunID); err != nil {
					return fmt.Errorf("record interrupted run state: %w", err)
				}
			}
		}
		output, err := CanonicalJSON(map[string]any{
			"operation":  operation,
			"outcome":    OutcomeInterrupted,
			"error_code": string(ErrInvocationInterruptedCode),
		})
		if err != nil {
			return err
		}
		actorJSON, err := marshalJSON("interrupted decision actor", actor)
		if err != nil {
			return err
		}
		hostJSON, err := marshalJSON("interrupted decision host", host)
		if err != nil {
			return err
		}
		key := "delivery.interrupted." + operation
		id := stableID("dec", projectID, deliveryRunID, key)
		_, err = tx.Exec(context.Background(), `INSERT OR IGNORE INTO delivery_decisions(
				decision_id, schema_version, record_version, project_id, delivery_run_id, decision_key, decision_kind,
				decided_by_json, inputs_fingerprint, output_json, alternatives_json, heuristic, policy_version,
				side_effect_class, created_at, created_by_json, host_json
			) VALUES (?, ?, 1, ?, ?, ?, 'terminal-outcome', ?, ?, ?, 'null', 0, ?, 'none', ?, ?, ?)`,
			id, SchemaDecision, projectID, deliveryRunID, key, actorJSON, proposal.AuthorizationFingerprint,
			string(output), proposal.PolicyVersion, CanonicalTimestamp(now), actorJSON, hostJSON)
		if err != nil {
			return fmt.Errorf("record interrupted decision: %w", err)
		}
		return nil
	})
}
