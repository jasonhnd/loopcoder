package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/budget"
	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

const (
	FallbackDecisionSchema = "loopcoder.fallback_decision.v1"
	ReplanDecisionSchema   = "loopcoder.replan_decision.v1"

	FallbackStatusSelected       = "selected"
	FallbackStatusReplanRequired = "replan-required"
	FallbackStatusNeedsHuman     = "needs-human"
	FallbackStatusBlocked        = "blocked"

	ReplanStatusPlanned       = "planned"
	ReplanStatusBlocked       = "blocked"
	ReplanStatusNeedsHuman    = "needs-human"
	ReplanStatusBoundExceeded = "bound-exceeded"
)

type FallbackTrigger string

const (
	FallbackTriggerCandidateFailed     FallbackTrigger = "candidate-failed"
	FallbackTriggerBreakerOpened       FallbackTrigger = "breaker-opened"
	FallbackTriggerQuotaExhausted      FallbackTrigger = "quota-exhausted"
	FallbackTriggerRateLimited         FallbackTrigger = "rate-limited"
	FallbackTriggerAuthExpired         FallbackTrigger = "auth-expired"
	FallbackTriggerModelRemoved        FallbackTrigger = "model-removed"
	FallbackTriggerBudgetRefused       FallbackTrigger = "budget-refused"
	FallbackTriggerVerificationFailed  FallbackTrigger = "verification-failed"
	FallbackTriggerWorkerFailed        FallbackTrigger = "worker-failed"
	FallbackTriggerTimeout             FallbackTrigger = "timeout"
	FallbackTriggerRequirementsChanged FallbackTrigger = "changed-requirements"
	FallbackTriggerUserRequested       FallbackTrigger = "user-requested"
)

type ReplanTrigger string

const (
	ReplanTriggerNoEligibleCandidate      ReplanTrigger = "no-eligible-candidate"
	ReplanTriggerLegalFallbackExhausted   ReplanTrigger = "legal-fallback-exhausted"
	ReplanTriggerScopeChangeNeeded        ReplanTrigger = "scope-change-needed"
	ReplanTriggerCapabilityGap            ReplanTrigger = "capability-gap"
	ReplanTriggerGraphBoundHit            ReplanTrigger = "graph-bound-hit"
	ReplanTriggerVerificationFailed       ReplanTrigger = "verification-failed"
	ReplanTriggerAmbiguousSideEffectState ReplanTrigger = "ambiguous-side-effect-state"
	ReplanTriggerUserChangedIntent        ReplanTrigger = "user-changed-intent"
	ReplanTriggerChangedRequirements      ReplanTrigger = "changed-requirements"
)

type LegalityResult struct {
	Dimension         string                     `json:"dimension"`
	MayDegrade        bool                       `json:"may_degrade"`
	Legal             bool                       `json:"legal"`
	ErrorCode         taskrequirements.ErrorCode `json:"error_code,omitempty"`
	Message           string                     `json:"message,omitempty"`
	EvidenceRecordIDs []string                   `json:"evidence_record_ids"`
}

type ChangedAuthorityInput struct {
	InputKind  string `json:"input_kind"`
	Previous   string `json:"previous,omitempty"`
	Current    string `json:"current,omitempty"`
	ApprovalID string `json:"approval_id,omitempty"`
}

type BoundsRemaining struct {
	MaxFallbacks     int `json:"max_fallbacks,omitempty"`
	FallbacksUsed    int `json:"fallbacks_used,omitempty"`
	FallbacksLeft    int `json:"fallbacks_left,omitempty"`
	MaxReplanPasses  int `json:"max_replan_passes,omitempty"`
	ReplansUsed      int `json:"replans_used,omitempty"`
	ReplanPassesLeft int `json:"replan_passes_left,omitempty"`
}

type FallbackDecision struct {
	SchemaVersion          string                     `json:"schema_version"`
	RecordVersion          int                        `json:"record_version"`
	FallbackDecisionID     string                     `json:"fallback_decision_id"`
	ProjectID              string                     `json:"project_id"`
	DeliveryRunID          string                     `json:"delivery_run_id"`
	TaskID                 string                     `json:"task_id"`
	RoutingDecisionID      string                     `json:"routing_decision_id"`
	FallbackOrdinal        int                        `json:"fallback_ordinal"`
	IdempotencyKey         string                     `json:"idempotency_key"`
	Trigger                FallbackTrigger            `json:"trigger"`
	PriorCandidateID       string                     `json:"prior_candidate_id"`
	FallbackCandidateID    string                     `json:"fallback_candidate_id,omitempty"`
	OriginalCandidateID    string                     `json:"original_candidate_id"`
	SelectedCandidateID    string                     `json:"selected_candidate_id,omitempty"`
	RouteLineage           []string                   `json:"route_lineage"`
	AttemptLineage         []string                   `json:"attempt_lineage"`
	LegalityResults        []LegalityResult           `json:"legality_results"`
	BoundsRemaining        BoundsRemaining            `json:"bounds_remaining"`
	ChangedAuthorityInputs []ChangedAuthorityInput    `json:"changed_authority_inputs"`
	ApprovalRequired       bool                       `json:"approval_required"`
	DecisionStatus         string                     `json:"decision_status"`
	RoutingFingerprint     string                     `json:"routing_fingerprint"`
	PolicyFingerprint      string                     `json:"policy_fingerprint"`
	PlanFingerprint        string                     `json:"plan_fingerprint"`
	CreatedAt              string                     `json:"created_at"`
	UpdatedAt              string                     `json:"updated_at"`
	DecidedBy              delivery.Actor             `json:"decided_by"`
	Host                   delivery.Host              `json:"host"`
	TerminalErrorCode      taskrequirements.ErrorCode `json:"terminal_error_code,omitempty"`
}

type ReplanDecision struct {
	SchemaVersion          string                     `json:"schema_version"`
	RecordVersion          int                        `json:"record_version"`
	ReplanDecisionID       string                     `json:"replan_decision_id"`
	ProjectID              string                     `json:"project_id"`
	DeliveryRunID          string                     `json:"delivery_run_id"`
	RoutingDecisionID      string                     `json:"routing_decision_id,omitempty"`
	ReplanOrdinal          int                        `json:"replan_ordinal"`
	IdempotencyKey         string                     `json:"idempotency_key"`
	Trigger                ReplanTrigger              `json:"trigger"`
	PriorPlanFingerprint   string                     `json:"prior_plan_fingerprint"`
	NewPlanFingerprint     string                     `json:"new_plan_fingerprint,omitempty"`
	RoutingFingerprint     string                     `json:"routing_fingerprint,omitempty"`
	BoundsRemaining        BoundsRemaining            `json:"bounds_remaining"`
	ChangedAuthorityInputs []ChangedAuthorityInput    `json:"changed_authority_inputs"`
	ApprovalRequired       bool                       `json:"approval_required"`
	AttemptLineage         []string                   `json:"attempt_lineage"`
	DecisionStatus         string                     `json:"decision_status"`
	CreatedAt              string                     `json:"created_at"`
	UpdatedAt              string                     `json:"updated_at"`
	DecidedBy              delivery.Actor             `json:"decided_by"`
	Host                   delivery.Host              `json:"host"`
	TerminalErrorCode      taskrequirements.ErrorCode `json:"terminal_error_code,omitempty"`
}

type FallbackInput struct {
	RoutingDecisionID         string
	Trigger                   FallbackTrigger
	PriorCandidateID          string
	IdempotencyKey            string
	Inputs                    Inputs
	AttemptLineage            []string
	ChangedAuthorityInputs    []ChangedAuthorityInput
	ApprovalRequired          bool
	Cancelled                 bool
	HeldBudgetReservationID   string
	HeldReservationGeneration int64
	DecidedBy                 delivery.Actor
	Host                      delivery.Host
}

type ReplanInput struct {
	ProjectID                 string
	DeliveryRunID             string
	RoutingDecisionID         string
	Trigger                   ReplanTrigger
	PriorPlanFingerprint      string
	NewPlanFingerprint        string
	RoutingFingerprint        string
	IdempotencyKey            string
	RoutingPolicyProfileID    string
	ChangedAuthorityInputs    []ChangedAuthorityInput
	ApprovalRequired          bool
	AttemptLineage            []string
	Cancelled                 bool
	HeldBudgetReservationID   string
	HeldReservationGeneration int64
	DecidedBy                 delivery.Actor
	Host                      delivery.Host
}

func DecideAndPersistFallback(ctx context.Context, store storage.Store, input FallbackInput) (FallbackDecision, error) {
	if store == nil {
		return FallbackDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "store is required"}
	}
	input.RoutingDecisionID = strings.TrimSpace(input.RoutingDecisionID)
	if input.RoutingDecisionID == "" || input.Trigger == "" || strings.TrimSpace(input.PriorCandidateID) == "" {
		return FallbackDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing decision, trigger, and prior candidate are required"}
	}
	if !validFallbackTrigger(input.Trigger) {
		return FallbackDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "unknown fallback trigger"}
	}
	if err := validateSchedulerActor(input.DecidedBy); err != nil {
		return FallbackDecision{}, err
	}
	if err := validateHost(input.Host); err != nil {
		return FallbackDecision{}, err
	}
	if input.ApprovalRequired || len(input.ChangedAuthorityInputs) > 0 {
		return FallbackDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "fallback cannot continue with changed authority or pending approval"}
	}
	original, err := LoadRoutingDecision(ctx, store, input.RoutingDecisionID)
	if err != nil {
		return FallbackDecision{}, err
	}
	if input.Cancelled {
		if err := releaseHeldReservationsOnCancellation(ctx, store, original.ProjectID, original.DeliveryRunID, input.HeldBudgetReservationID, input.HeldReservationGeneration, input.DecidedBy, input.Host); err != nil {
			return FallbackDecision{}, err
		}
		return FallbackDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "fallback stopped by cancellation before route selection"}
	}
	cancelled, err := isDeliveryRunCancelled(ctx, store, original.ProjectID, original.DeliveryRunID)
	if err != nil {
		return FallbackDecision{}, err
	}
	if cancelled {
		if err := releaseHeldReservationsOnCancellation(ctx, store, original.ProjectID, original.DeliveryRunID, input.HeldBudgetReservationID, input.HeldReservationGeneration, input.DecidedBy, input.Host); err != nil {
			return FallbackDecision{}, err
		}
		return FallbackDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "fallback stopped by durable cancellation before route selection"}
	}
	var stored FallbackDecision
	cancelledInTx := false
	err = store.WithWriteTx(ctx, func(tx storage.Tx) error {
		original, err := loadRoutingDecisionTx(ctx, tx, input.RoutingDecisionID)
		if err != nil {
			return err
		}
		profile, err := resolveFallbackProfileTx(ctx, tx, original)
		if err != nil {
			return err
		}
		storedReq, err := loadTaskRequirementTx(ctx, tx, original.TaskRequirementID)
		if err != nil {
			return err
		}
		if err := validateFallbackInputs(original, profile, storedReq, input.Inputs); err != nil {
			return err
		}
		cancelled, err := deliveryRunCancelledTx(ctx, tx, original.ProjectID, original.DeliveryRunID)
		if err != nil {
			return err
		}
		if cancelled {
			cancelledInTx = true
			return &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "fallback stopped by durable cancellation at transaction boundary"}
		}
		chain, err := loadFallbackChainTx(ctx, tx, original.RoutingDecisionID)
		if err != nil {
			return err
		}
		pins, err := pinsFromDecisionTx(ctx, tx, original, input.Inputs.Pins)
		if err != nil {
			return err
		}
		idempotencyKey, err := fallbackIdempotencyKey(input, original, profile, storedReq, pins)
		if err != nil {
			return err
		}
		if existing, ok, err := loadFallbackByIdempotency(ctx, tx, original.RoutingDecisionID, idempotencyKey); err != nil || ok {
			if ok && (existing.Trigger != input.Trigger || existing.PriorCandidateID != strings.TrimSpace(input.PriorCandidateID)) {
				return &delivery.TypedError{Code: delivery.ErrDuplicateReplayCode, Message: "fallback idempotency key replayed with different request"}
			}
			stored = existing
			return err
		}
		latest := latestFallbackCandidate(original, chain)
		if strings.TrimSpace(input.PriorCandidateID) != latest {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "fallback prior candidate does not match latest persisted route lineage"}
		}
		evaluated := input.Inputs
		evaluated.Requirement = storedReq
		evaluated.Policy = profile.EligibilityPolicy
		evaluated.Pins = pins
		originalCandidateCount := len(evaluated.Candidates)
		evaluated.Candidates = excludeFallbackCandidates(evaluated.Candidates, attemptedFallbackCandidates(original, chain, input.PriorCandidateID))
		eligibility := Result{}
		if originalCandidateCount == 0 || len(evaluated.Candidates) > 0 {
			eligibility = FilterHardEligibility(evaluated)
		}
		scored := scoreCandidates(eligibility.Eligible, evaluated, profile.OptimizationPolicy)
		selected := Candidate{}
		if len(scored) > 0 {
			selected = scored[0].Candidate
		}
		fingerprint, err := fallbackRoutingFingerprint(original, profile, input, storedReq, pins, eligibility, selected)
		if err != nil {
			return err
		}
		used := len(chain)
		decision := buildFallbackDecision(original, profile, input, selected, eligibility, fingerprint, idempotencyKey, used+1, store.Now())
		if used >= profile.FallbackPolicy.MaxFallbacks {
			decision.FallbackCandidateID = ""
			decision.SelectedCandidateID = ""
			decision.DecisionStatus = FallbackStatusReplanRequired
			decision.TerminalErrorCode = taskrequirements.ErrReplanRequiredCode
			markLegality(&decision, "fallback-bound", false, taskrequirements.ErrReplanRequiredCode, "max_fallbacks exhausted")
		} else if selected.RoutingCandidateID == "" {
			decision.DecisionStatus = FallbackStatusReplanRequired
			decision.TerminalErrorCode = taskrequirements.ErrReplanRequiredCode
		}
		if err := insertFallbackDecisionTx(ctx, tx, decision); err != nil {
			return err
		}
		stored = decision
		return nil
	})
	if err != nil {
		if cancelledInTx {
			if releaseErr := releaseHeldReservationsOnCancellation(ctx, store, original.ProjectID, original.DeliveryRunID, input.HeldBudgetReservationID, input.HeldReservationGeneration, input.DecidedBy, input.Host); releaseErr != nil {
				return FallbackDecision{}, releaseErr
			}
		}
		return FallbackDecision{}, err
	}
	if stored.TerminalErrorCode != "" {
		return stored, routingTerminalError(stored.TerminalErrorCode)
	}
	return stored, nil
}

func LoadFallbackDecision(ctx context.Context, store storage.Store, id string) (FallbackDecision, error) {
	var payload string
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT payload_json FROM fallback_decisions WHERE fallback_decision_id = ?`, strings.TrimSpace(id)).Scan(&payload)
	})
	if err != nil {
		return FallbackDecision{}, err
	}
	var decision FallbackDecision
	if err := json.Unmarshal([]byte(payload), &decision); err != nil {
		return FallbackDecision{}, err
	}
	return decision, nil
}

func DecideAndPersistReplan(ctx context.Context, store storage.Store, input ReplanInput) (ReplanDecision, error) {
	if store == nil {
		return ReplanDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "store is required"}
	}
	if err := validateSchedulerActor(input.DecidedBy); err != nil {
		return ReplanDecision{}, err
	}
	if err := validateHost(input.Host); err != nil {
		return ReplanDecision{}, err
	}
	input.ProjectID = strings.TrimSpace(input.ProjectID)
	input.DeliveryRunID = strings.TrimSpace(input.DeliveryRunID)
	input.PriorPlanFingerprint = strings.TrimSpace(input.PriorPlanFingerprint)
	if input.ProjectID == "" || input.DeliveryRunID == "" || input.Trigger == "" || !validFingerprint(input.PriorPlanFingerprint) {
		return ReplanDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "project, delivery run, trigger, and prior plan fingerprint are required"}
	}
	if !validReplanTrigger(input.Trigger) {
		return ReplanDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "unknown replan trigger"}
	}
	if input.Cancelled {
		if err := releaseHeldReservationsOnCancellation(ctx, store, input.ProjectID, input.DeliveryRunID, input.HeldBudgetReservationID, input.HeldReservationGeneration, input.DecidedBy, input.Host); err != nil {
			return ReplanDecision{}, err
		}
		return ReplanDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "replan stopped by cancellation before planning"}
	}
	var stored ReplanDecision
	cancelledInTx := false
	err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		run, err := loadReplanRunTx(ctx, tx, input.ProjectID, input.DeliveryRunID)
		if err != nil {
			return err
		}
		if run.PlanFingerprint != input.PriorPlanFingerprint {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "replan prior plan fingerprint does not match current delivery run"}
		}
		profile, route, err := resolveReplanRouteProfileTx(ctx, tx, input, run)
		if err != nil {
			return err
		}
		resolvedInput := input
		resolvedInput.RoutingDecisionID = route.RoutingDecisionID
		resolvedInput.RoutingPolicyProfileID = profile.RoutingPolicyProfileID
		if strings.TrimSpace(resolvedInput.RoutingFingerprint) == "" {
			resolvedInput.RoutingFingerprint = route.RoutingFingerprint
		}
		cancelled, err := deliveryRunCancelledTx(ctx, tx, input.ProjectID, input.DeliveryRunID)
		if err != nil {
			return err
		}
		if cancelled {
			cancelledInTx = true
			return &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "replan stopped by durable cancellation at transaction boundary"}
		}
		idempotencyKey, err := replanIdempotencyKey(resolvedInput, run, profile)
		if err != nil {
			return err
		}
		if existing, ok, err := loadReplanByIdempotency(ctx, tx, input.DeliveryRunID, idempotencyKey); err != nil || ok {
			if ok && (existing.Trigger != input.Trigger || existing.PriorPlanFingerprint != input.PriorPlanFingerprint || existing.NewPlanFingerprint != strings.TrimSpace(input.NewPlanFingerprint) || existing.RoutingFingerprint != strings.TrimSpace(resolvedInput.RoutingFingerprint)) {
				return &delivery.TypedError{Code: delivery.ErrDuplicateReplayCode, Message: "replan idempotency key replayed with different request"}
			}
			stored = existing
			return err
		}
		used, err := countReplansTx(ctx, tx, input.ProjectID, input.DeliveryRunID)
		if err != nil {
			return err
		}
		decision := buildReplanDecision(profile, resolvedInput, idempotencyKey, used+1, store.Now())
		if newPlan := strings.TrimSpace(resolvedInput.NewPlanFingerprint); newPlan != "" && newPlan != run.PlanFingerprint {
			decision.ApprovalRequired = true
		}
		if decision.ApprovalRequired {
			decision.DecisionStatus = ReplanStatusNeedsHuman
			decision.TerminalErrorCode = taskrequirements.ErrorCode(delivery.ErrApprovalRequiredCode)
		}
		if used >= profile.ReplanPolicy.MaxReplanPasses {
			decision.DecisionStatus = ReplanStatusBoundExceeded
			decision.TerminalErrorCode = taskrequirements.ErrReplanBoundExceededCode
			decision.NewPlanFingerprint = ""
		}
		if err := insertReplanDecisionTx(ctx, tx, decision); err != nil {
			return err
		}
		stored = decision
		return nil
	})
	if err != nil {
		if cancelledInTx {
			if releaseErr := releaseHeldReservationsOnCancellation(ctx, store, input.ProjectID, input.DeliveryRunID, input.HeldBudgetReservationID, input.HeldReservationGeneration, input.DecidedBy, input.Host); releaseErr != nil {
				return ReplanDecision{}, releaseErr
			}
		}
		return ReplanDecision{}, err
	}
	if stored.TerminalErrorCode != "" {
		return stored, routingTerminalError(stored.TerminalErrorCode)
	}
	return stored, nil
}

func LoadReplanDecision(ctx context.Context, store storage.Store, id string) (ReplanDecision, error) {
	var payload string
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT payload_json FROM replan_decisions WHERE replan_decision_id = ?`, strings.TrimSpace(id)).Scan(&payload)
	})
	if err != nil {
		return ReplanDecision{}, err
	}
	var decision ReplanDecision
	if err := json.Unmarshal([]byte(payload), &decision); err != nil {
		return ReplanDecision{}, err
	}
	return decision, nil
}

func buildFallbackDecision(original RoutingDecision, profile RoutingPolicyProfile, input FallbackInput, selected Candidate, eligibility Result, fingerprint, idempotencyKey string, ordinal int, now time.Time) FallbackDecision {
	nowText := delivery.CanonicalTimestamp(now)
	selectedID := selected.RoutingCandidateID
	status := FallbackStatusSelected
	var terminal taskrequirements.ErrorCode
	if selectedID == "" {
		status = FallbackStatusReplanRequired
		terminal = taskrequirements.ErrReplanRequiredCode
	}
	bounds := BoundsRemaining{
		MaxFallbacks:  profile.FallbackPolicy.MaxFallbacks,
		FallbacksUsed: ordinal,
		FallbacksLeft: maxInt(0, profile.FallbackPolicy.MaxFallbacks-ordinal),
	}
	decision := FallbackDecision{
		SchemaVersion:          FallbackDecisionSchema,
		RecordVersion:          1,
		ProjectID:              original.ProjectID,
		DeliveryRunID:          original.DeliveryRunID,
		TaskID:                 original.TaskID,
		RoutingDecisionID:      original.RoutingDecisionID,
		FallbackOrdinal:        ordinal,
		IdempotencyKey:         idempotencyKey,
		Trigger:                input.Trigger,
		PriorCandidateID:       strings.TrimSpace(input.PriorCandidateID),
		FallbackCandidateID:    selectedID,
		OriginalCandidateID:    original.ChosenCandidateID,
		SelectedCandidateID:    selectedID,
		RouteLineage:           nonNilStrings(append(append([]string{original.ChosenCandidateID}, original.FallbackChain...), selectedID)),
		AttemptLineage:         nonNilStrings(input.AttemptLineage),
		LegalityResults:        fallbackLegalityResults(original, profile, input, selected, eligibility),
		BoundsRemaining:        bounds,
		ChangedAuthorityInputs: nonNilAuthorityInputs(input.ChangedAuthorityInputs),
		ApprovalRequired:       input.ApprovalRequired,
		DecisionStatus:         status,
		RoutingFingerprint:     fingerprint,
		PolicyFingerprint:      original.PolicyFingerprint,
		PlanFingerprint:        original.PlanFingerprint,
		CreatedAt:              nowText,
		UpdatedAt:              nowText,
		DecidedBy:              input.DecidedBy,
		Host:                   input.Host,
		TerminalErrorCode:      terminal,
	}
	decision.FallbackDecisionID = fallbackDecisionID(original.RoutingDecisionID, ordinal, fingerprint)
	return decision
}

func fallbackLegalityResults(original RoutingDecision, profile RoutingPolicyProfile, input FallbackInput, selected Candidate, eligibility Result) []LegalityResult {
	rows := fallbackMatrixRows()
	if selected.RoutingCandidateID == "" {
		failures := map[RejectionCode][]RejectionReason{}
		for _, rejected := range eligibility.Rejected {
			for _, reason := range rejected.Reasons {
				failures[reason.Code] = append(failures[reason.Code], reason)
			}
		}
		for i := range rows {
			if reason := firstReasonForDimension(rows[i].Dimension, failures); reason != nil {
				rows[i].Legal = false
				rows[i].ErrorCode = reason.ErrorCode
				rows[i].Message = reason.Message
				rows[i].EvidenceRecordIDs = dedupeStrings(append(reason.EvidenceRecordIDs, reason.SnapshotIDs...))
			}
		}
	}
	for i := range rows {
		switch rows[i].Dimension {
		case "plan_graph":
			if input.Inputs.Requirement.PlanFingerprint != original.PlanFingerprint || input.Inputs.Requirement.TaskRequirementID != original.TaskRequirementID {
				rows[i].Legal = false
				rows[i].ErrorCode = taskrequirements.ErrReplanRequiredCode
				rows[i].Message = "task requirement or plan graph changed; replan required"
			}
		case "permission_enforcement":
			if selected.RoutingCandidateID != "" && permissionRank(selected.Permission) < permissionRank(input.Inputs.Requirement.PermissionRequired) {
				rows[i].Legal = false
				rows[i].ErrorCode = taskrequirements.ErrFallbackWouldWeakenPolicyCode
				rows[i].Message = "fallback would weaken permission enforcement"
			}
		case "side_effect_class":
			if selected.RoutingCandidateID != "" && sideEffectRank(selected.LaunchSideEffectClass) > sideEffectRank(input.Inputs.Requirement.SideEffectClass) {
				rows[i].Legal = false
				rows[i].ErrorCode = taskrequirements.ErrFallbackWouldWeakenPolicyCode
				rows[i].Message = "fallback would raise side-effect class"
			}
		case "user_pin":
			if profile.FallbackPolicy.NeverIgnoreUserPin && len(original.UserPinRefs) > 0 && selected.RoutingCandidateID == "" {
				rows[i].Legal = false
				rows[i].ErrorCode = taskrequirements.ErrFallbackWouldWeakenPolicyCode
				rows[i].Message = "no pinned hard-eligible fallback candidate exists"
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Dimension < rows[j].Dimension })
	return rows
}

func fallbackMatrixRows() []LegalityResult {
	type row struct {
		dim string
		may bool
	}
	rows := []row{
		{"permission_enforcement", false}, {"minimum_capability", false}, {"side_effect_class", false},
		{"scope", false}, {"verifier_independence", false}, {"verification_requirement", false},
		{"risk_tier", false}, {"auth_readiness", false}, {"quota_confidence", false},
		{"budget_ceiling", false}, {"circuit_breaker", false}, {"provider_model_identity", true},
		{"availability_score", true}, {"quota_headroom", true}, {"cost", true}, {"latency", true},
		{"diversity", true}, {"user_pin", false}, {"plan_graph", false},
	}
	out := make([]LegalityResult, 0, len(rows))
	for _, row := range rows {
		out = append(out, LegalityResult{Dimension: row.dim, MayDegrade: row.may, Legal: true, EvidenceRecordIDs: []string{}})
	}
	return out
}

func firstReasonForDimension(dimension string, failures map[RejectionCode][]RejectionReason) *RejectionReason {
	for _, code := range rejectionCodesForDimension(dimension) {
		if reasons := failures[code]; len(reasons) > 0 {
			return &reasons[0]
		}
	}
	return nil
}

func rejectionCodesForDimension(dimension string) []RejectionCode {
	switch dimension {
	case "permission_enforcement":
		return []RejectionCode{RejectPermissionUnsupported, RejectReadOnlyUnsupported}
	case "minimum_capability":
		return []RejectionCode{RejectRoleUnsupported, RejectReadOnlyUnsupported, RejectJSONOutputUnsupported, RejectNestedSubagentsUnsupported, RejectMCPConfigUnsupported, RejectCancellationUnsupported, RejectUsageReportingUnsupported, RejectContextWindowInsufficient, RejectToolSupportUnsupported, RejectImageInputUnsupported, RejectImageOutputUnsupported, RejectModelUnavailable, RejectUnknownRecordVersion}
	case "side_effect_class":
		return []RejectionCode{RejectSideEffectClassExceeded}
	case "scope":
		return []RejectionCode{RejectScopeUnsupported, RejectNetworkPermissionMissing, RejectDataClassificationUnsupported}
	case "verifier_independence":
		return []RejectionCode{RejectVerifierIndependenceInsufficient}
	case "verification_requirement":
		return []RejectionCode{RejectVerificationRouteMissing}
	case "risk_tier":
		return []RejectionCode{RejectRiskTierUnsupported, RejectQualityFloorInsufficient}
	case "auth_readiness":
		return []RejectionCode{RejectAuthNotReady, RejectAccountProfileAmbiguous}
	case "quota_confidence", "quota_headroom":
		return []RejectionCode{RejectQuotaConfidenceInsufficient, RejectQuotaExhausted, RejectEvidenceStale, RejectUnknownTelemetry}
	case "budget_ceiling", "cost":
		return []RejectionCode{RejectBudgetExhausted}
	case "circuit_breaker":
		return []RejectionCode{RejectBreakerOpen}
	case "provider_model_identity", "user_pin":
		return []RejectionCode{RejectPinnedCandidateNotMatched, RejectPinnedCandidateIneligible, RejectCandidateExcluded}
	case "availability_score":
		return []RejectionCode{RejectAvailabilityHardIneligible}
	case "latency":
		return []RejectionCode{RejectCancellationUnsupported}
	case "diversity":
		return []RejectionCode{RejectVerifierIndependenceInsufficient}
	default:
		return nil
	}
}

func buildReplanDecision(profile RoutingPolicyProfile, input ReplanInput, idempotencyKey string, ordinal int, now time.Time) ReplanDecision {
	nowText := delivery.CanonicalTimestamp(now)
	changed := nonNilAuthorityInputs(input.ChangedAuthorityInputs)
	approvalRequired := input.ApprovalRequired || (profile.ReplanPolicy.RequireFreshApproval && len(changed) > 0)
	status := ReplanStatusPlanned
	var terminal taskrequirements.ErrorCode
	if input.NewPlanFingerprint == "" {
		status = ReplanStatusNeedsHuman
		terminal = taskrequirements.ErrReplanRequiredCode
	}
	bounds := BoundsRemaining{
		MaxReplanPasses:  profile.ReplanPolicy.MaxReplanPasses,
		ReplansUsed:      ordinal,
		ReplanPassesLeft: maxInt(0, profile.ReplanPolicy.MaxReplanPasses-ordinal),
	}
	decision := ReplanDecision{
		SchemaVersion:          ReplanDecisionSchema,
		RecordVersion:          1,
		ProjectID:              input.ProjectID,
		DeliveryRunID:          input.DeliveryRunID,
		RoutingDecisionID:      strings.TrimSpace(input.RoutingDecisionID),
		ReplanOrdinal:          ordinal,
		IdempotencyKey:         idempotencyKey,
		Trigger:                input.Trigger,
		PriorPlanFingerprint:   input.PriorPlanFingerprint,
		NewPlanFingerprint:     strings.TrimSpace(input.NewPlanFingerprint),
		RoutingFingerprint:     strings.TrimSpace(input.RoutingFingerprint),
		BoundsRemaining:        bounds,
		ChangedAuthorityInputs: changed,
		ApprovalRequired:       approvalRequired,
		AttemptLineage:         nonNilStrings(input.AttemptLineage),
		DecisionStatus:         status,
		CreatedAt:              nowText,
		UpdatedAt:              nowText,
		DecidedBy:              input.DecidedBy,
		Host:                   input.Host,
		TerminalErrorCode:      terminal,
	}
	decision.ReplanDecisionID = replanDecisionID(decision.ProjectID, decision.DeliveryRunID, decision.PriorPlanFingerprint, ordinal)
	return decision
}

func insertFallbackDecisionTx(ctx context.Context, tx storage.Tx, decision FallbackDecision) error {
	payload, err := delivery.CanonicalJSON(decision)
	if err != nil {
		return err
	}
	legality, err := canonicalString(decision.LegalityResults)
	if err != nil {
		return err
	}
	attempts, err := canonicalString(decision.AttemptLineage)
	if err != nil {
		return err
	}
	changed, err := canonicalString(decision.ChangedAuthorityInputs)
	if err != nil {
		return err
	}
	actor, err := canonicalString(decision.DecidedBy)
	if err != nil {
		return err
	}
	host, err := canonicalString(decision.Host)
	if err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `INSERT INTO fallback_decisions(
		fallback_decision_id, schema_version, record_version, project_id, delivery_run_id, task_id, routing_decision_id,
		fallback_ordinal, idempotency_key, trigger, prior_candidate_id, fallback_candidate_id, decision_status,
		terminal_error_code, routing_fingerprint, original_candidate_id, selected_candidate_id, bounds_remaining,
		approval_required, legality_results_json, attempt_lineage_json, changed_authority_inputs_json,
		payload_json, created_at, updated_at, decided_by_json, host_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(fallback_decision_id) DO NOTHING`,
		decision.FallbackDecisionID, decision.SchemaVersion, decision.RecordVersion, decision.ProjectID, decision.DeliveryRunID, decision.TaskID, decision.RoutingDecisionID,
		decision.FallbackOrdinal, decision.IdempotencyKey, string(decision.Trigger), decision.PriorCandidateID, decision.FallbackCandidateID, decision.DecisionStatus,
		string(decision.TerminalErrorCode), decision.RoutingFingerprint, decision.OriginalCandidateID, decision.SelectedCandidateID, decision.BoundsRemaining.FallbacksLeft,
		boolInt(decision.ApprovalRequired), legality, attempts, changed, string(payload), decision.CreatedAt, decision.UpdatedAt, actor, host)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 0 {
		return err
	}
	var existing string
	if err := tx.QueryRow(ctx, `SELECT payload_json FROM fallback_decisions WHERE fallback_decision_id = ?`, decision.FallbackDecisionID).Scan(&existing); err != nil {
		return err
	}
	if existing != string(payload) {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrDuplicateRecordCode, Message: "fallback decision id already exists with different payload"}
	}
	return nil
}

func insertReplanDecisionTx(ctx context.Context, tx storage.Tx, decision ReplanDecision) error {
	payload, err := delivery.CanonicalJSON(decision)
	if err != nil {
		return err
	}
	changed, err := canonicalString(decision.ChangedAuthorityInputs)
	if err != nil {
		return err
	}
	attempts, err := canonicalString(decision.AttemptLineage)
	if err != nil {
		return err
	}
	actor, err := canonicalString(decision.DecidedBy)
	if err != nil {
		return err
	}
	host, err := canonicalString(decision.Host)
	if err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `INSERT INTO replan_decisions(
		replan_decision_id, schema_version, record_version, project_id, delivery_run_id, routing_decision_id,
		replan_ordinal, idempotency_key, trigger, prior_plan_fingerprint, new_plan_fingerprint, routing_fingerprint,
		bounds_remaining, approval_required, decision_status, terminal_error_code, changed_authority_inputs_json,
		attempt_lineage_json, payload_json, created_at, updated_at, decided_by_json, host_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(replan_decision_id) DO NOTHING`,
		decision.ReplanDecisionID, decision.SchemaVersion, decision.RecordVersion, decision.ProjectID, decision.DeliveryRunID, decision.RoutingDecisionID,
		decision.ReplanOrdinal, decision.IdempotencyKey, string(decision.Trigger), decision.PriorPlanFingerprint, decision.NewPlanFingerprint, decision.RoutingFingerprint,
		decision.BoundsRemaining.ReplanPassesLeft, boolInt(decision.ApprovalRequired), decision.DecisionStatus, string(decision.TerminalErrorCode), changed,
		attempts, string(payload), decision.CreatedAt, decision.UpdatedAt, actor, host)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 0 {
		return err
	}
	var existing string
	if err := tx.QueryRow(ctx, `SELECT payload_json FROM replan_decisions WHERE replan_decision_id = ?`, decision.ReplanDecisionID).Scan(&existing); err != nil {
		return err
	}
	if existing != string(payload) {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrDuplicateRecordCode, Message: "replan decision id already exists with different payload"}
	}
	return nil
}

func loadRoutingDecisionTx(ctx context.Context, tx storage.Tx, routingDecisionID string) (RoutingDecision, error) {
	var payload string
	err := tx.QueryRow(ctx, `SELECT payload_json FROM routing_decisions WHERE routing_decision_id = ?`, strings.TrimSpace(routingDecisionID)).Scan(&payload)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RoutingDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrMissingReferenceCode, Message: "routing decision was not persisted"}
		}
		return RoutingDecision{}, err
	}
	var decision RoutingDecision
	if err := json.Unmarshal([]byte(payload), &decision); err != nil {
		return RoutingDecision{}, err
	}
	if err := validateRoutingDecision(decision); err != nil {
		return RoutingDecision{}, err
	}
	return decision, nil
}

func loadTaskRequirementTx(ctx context.Context, tx storage.Tx, taskRequirementID string) (taskrequirements.TaskRequirement, error) {
	var payload string
	err := tx.QueryRow(ctx, `SELECT payload_json FROM task_requirements WHERE task_requirement_id = ?`, strings.TrimSpace(taskRequirementID)).Scan(&payload)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return taskrequirements.TaskRequirement{}, &taskrequirements.TypedError{Code: taskrequirements.ErrMissingReferenceCode, Message: "task requirement was not persisted"}
		}
		return taskrequirements.TaskRequirement{}, err
	}
	var req taskrequirements.TaskRequirement
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		return taskrequirements.TaskRequirement{}, err
	}
	if err := taskrequirements.Validate(req); err != nil {
		return taskrequirements.TaskRequirement{}, err
	}
	return req, nil
}

func loadRoutingPolicyProfileTx(ctx context.Context, tx storage.Tx, id string) (RoutingPolicyProfile, error) {
	var payload string
	err := tx.QueryRow(ctx, `SELECT payload_json FROM routing_policy_profiles WHERE routing_policy_profile_id = ?`, strings.TrimSpace(id)).Scan(&payload)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RoutingPolicyProfile{}, &taskrequirements.TypedError{Code: taskrequirements.ErrMissingReferenceCode, Message: "routing policy profile was not persisted"}
		}
		return RoutingPolicyProfile{}, err
	}
	var profile RoutingPolicyProfile
	if err := json.Unmarshal([]byte(payload), &profile); err != nil {
		return RoutingPolicyProfile{}, err
	}
	if err := ValidateRoutingPolicyProfile(profile); err != nil {
		return RoutingPolicyProfile{}, err
	}
	return profile, nil
}

func resolveFallbackProfileTx(ctx context.Context, tx storage.Tx, original RoutingDecision) (RoutingPolicyProfile, error) {
	profile, err := loadRoutingPolicyProfileTx(ctx, tx, original.RoutingPolicyProfileID)
	if err != nil {
		return RoutingPolicyProfile{}, err
	}
	if original.RoutingPolicyProfile != nil {
		embedded := normalizeRoutingPolicyProfile(*original.RoutingPolicyProfile)
		if err := requireSameRoutingPolicyProfile(embedded, profile); err != nil {
			return RoutingPolicyProfile{}, err
		}
	}
	if profile.PolicyFingerprint != original.PolicyFingerprint {
		return RoutingPolicyProfile{}, &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "fallback profile fingerprint does not match original route"}
	}
	return profile, nil
}

func resolveReplanRouteProfileTx(ctx context.Context, tx storage.Tx, input ReplanInput, run replanRunAuthority) (RoutingPolicyProfile, RoutingDecision, error) {
	var route RoutingDecision
	var err error
	if strings.TrimSpace(input.RoutingDecisionID) != "" {
		route, err = loadRoutingDecisionTx(ctx, tx, input.RoutingDecisionID)
	} else {
		route, err = loadLatestRoutingDecisionForRunTx(ctx, tx, run.ProjectID, run.DeliveryRunID)
	}
	if err != nil {
		return RoutingPolicyProfile{}, RoutingDecision{}, err
	}
	if route.ProjectID != run.ProjectID || route.DeliveryRunID != run.DeliveryRunID || route.PlanFingerprint != run.PlanFingerprint {
		return RoutingPolicyProfile{}, RoutingDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "replan route does not match current delivery run"}
	}
	if strings.TrimSpace(input.RoutingPolicyProfileID) != "" && strings.TrimSpace(input.RoutingPolicyProfileID) != route.RoutingPolicyProfileID {
		return RoutingPolicyProfile{}, RoutingDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "replan routing policy profile does not match current route"}
	}
	if strings.TrimSpace(input.RoutingFingerprint) != "" && strings.TrimSpace(input.RoutingFingerprint) != route.RoutingFingerprint {
		return RoutingPolicyProfile{}, RoutingDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "replan routing fingerprint does not match current route"}
	}
	profile, err := loadRoutingPolicyProfileTx(ctx, tx, route.RoutingPolicyProfileID)
	if err != nil {
		return RoutingPolicyProfile{}, RoutingDecision{}, err
	}
	if profile.PolicyFingerprint != route.PolicyFingerprint {
		return RoutingPolicyProfile{}, RoutingDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "replan profile fingerprint does not match current route"}
	}
	return profile, route, nil
}

func loadLatestRoutingDecisionForRunTx(ctx context.Context, tx storage.Tx, projectID, deliveryRunID string) (RoutingDecision, error) {
	rows, err := tx.Query(ctx, `SELECT payload_json FROM routing_decisions
		WHERE project_id = ? AND delivery_run_id = ?
		ORDER BY created_at DESC, routing_decision_id DESC LIMIT 2`, strings.TrimSpace(projectID), strings.TrimSpace(deliveryRunID))
	if err != nil {
		return RoutingDecision{}, err
	}
	defer rows.Close()
	var payloads []string
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return RoutingDecision{}, err
		}
		payloads = append(payloads, payload)
	}
	if err := rows.Err(); err != nil {
		return RoutingDecision{}, err
	}
	if len(payloads) == 0 {
		return RoutingDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrMissingReferenceCode, Message: "replan current route was not persisted"}
	}
	if len(payloads) > 1 {
		return RoutingDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "replan current route is ambiguous without an exact routing decision"}
	}
	var decision RoutingDecision
	if err := json.Unmarshal([]byte(payloads[0]), &decision); err != nil {
		return RoutingDecision{}, err
	}
	if err := validateRoutingDecision(decision); err != nil {
		return RoutingDecision{}, err
	}
	return decision, nil
}

func validateFallbackInputs(original RoutingDecision, profile RoutingPolicyProfile, storedReq taskrequirements.TaskRequirement, inputs Inputs) error {
	if original.DecisionStatus != DecisionStatusSelected || original.ChosenCandidateID == "" {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "fallback requires an original selected route"}
	}
	if storedReq.ProjectID != original.ProjectID || storedReq.DeliveryRunID != original.DeliveryRunID ||
		storedReq.TaskID != original.TaskID || storedReq.TaskRequirementID != original.TaskRequirementID {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "stored fallback requirement does not match original route"}
	}
	if storedReq.PlanFingerprint != original.PlanFingerprint {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "stored fallback requirement does not match original plan fingerprint"}
	}
	caller := inputs.Requirement
	if caller.ProjectID != storedReq.ProjectID || caller.DeliveryRunID != storedReq.DeliveryRunID ||
		caller.TaskID != storedReq.TaskID || caller.TaskRequirementID != storedReq.TaskRequirementID {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "fallback requirement does not match original route"}
	}
	if caller.PlanFingerprint != storedReq.PlanFingerprint {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "fallback cannot change plan fingerprint"}
	}
	if !sameCanonicalRequirementAuthority(caller, storedReq) {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrFallbackWouldWeakenPolicyCode, Message: "caller fallback requirement does not match persisted authority"}
	}
	if profile.PolicyFingerprint != original.PolicyFingerprint {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "fallback profile fingerprint does not match original route"}
	}
	return nil
}

func fallbackRoutingFingerprint(original RoutingDecision, profile RoutingPolicyProfile, input FallbackInput, req taskrequirements.TaskRequirement, pins []Pin, eligibility Result, selected Candidate) (string, error) {
	digest, _, err := delivery.DigestCanonicalJSON(map[string]any{
		"routing_decision_id":   original.RoutingDecisionID,
		"original_fingerprint":  original.RoutingFingerprint,
		"policy_fingerprint":    profile.PolicyFingerprint,
		"requirement_id":        req.TaskRequirementID,
		"requirement_fp":        req.TaskRequirementFingerprint,
		"user_pins":             pins,
		"trigger":               input.Trigger,
		"prior_candidate_id":    input.PriorCandidateID,
		"selected_candidate_id": selected.RoutingCandidateID,
		"eligible_candidates":   eligibility.Eligible,
		"rejected_candidates":   eligibility.Rejected,
	})
	return digest, err
}

func fallbackIdempotencyKey(input FallbackInput, original RoutingDecision, profile RoutingPolicyProfile, req taskrequirements.TaskRequirement, pins []Pin) (string, error) {
	digest, _, err := delivery.DigestCanonicalJSON(map[string]any{
		"schema_version":      "loopcoder.fallback_event.v1",
		"routing_decision_id": original.RoutingDecisionID,
		"routing_fingerprint": original.RoutingFingerprint,
		"policy_fingerprint":  profile.PolicyFingerprint,
		"requirement_id":      req.TaskRequirementID,
		"requirement_fp":      req.TaskRequirementFingerprint,
		"trigger":             input.Trigger,
		"prior_candidate_id":  strings.TrimSpace(input.PriorCandidateID),
		"user_pins":           pins,
	})
	if err != nil {
		return "", err
	}
	return "fallback-event:" + digest, nil
}

func replanIdempotencyKey(input ReplanInput, run replanRunAuthority, profile RoutingPolicyProfile) (string, error) {
	digest, _, err := delivery.DigestCanonicalJSON(map[string]any{
		"schema_version":         "loopcoder.replan_event.v1",
		"project_id":             run.ProjectID,
		"delivery_run_id":        run.DeliveryRunID,
		"authorization_fp":       run.AuthorizationFingerprint,
		"input_fp":               run.InputFingerprint,
		"policy_fp":              run.PolicyFingerprint,
		"prior_plan_fp":          input.PriorPlanFingerprint,
		"new_plan_fp":            strings.TrimSpace(input.NewPlanFingerprint),
		"routing_decision_id":    strings.TrimSpace(input.RoutingDecisionID),
		"routing_fingerprint":    strings.TrimSpace(input.RoutingFingerprint),
		"routing_policy_profile": profile.RoutingPolicyProfileID,
		"routing_policy_fp":      profile.PolicyFingerprint,
		"trigger":                input.Trigger,
	})
	if err != nil {
		return "", err
	}
	return "replan-event:" + digest, nil
}

func fallbackDecisionID(routingDecisionID string, ordinal int, routingFingerprint string) string {
	return "fdec_" + digestBase32(routingDecisionID, fmt.Sprintf("%d", ordinal), routingFingerprint)
}

func replanDecisionID(projectID, deliveryRunID, priorPlanFingerprint string, ordinal int) string {
	return "replan_" + digestBase32(projectID, deliveryRunID, priorPlanFingerprint, fmt.Sprintf("%d", ordinal))
}

func countReplansTx(ctx context.Context, tx storage.Tx, projectID, deliveryRunID string) (int, error) {
	var count int
	err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM replan_decisions WHERE project_id = ? AND delivery_run_id = ?`, projectID, deliveryRunID).Scan(&count)
	return count, err
}

func loadFallbackByIdempotency(ctx context.Context, tx storage.Tx, routingDecisionID, key string) (FallbackDecision, bool, error) {
	var payload string
	err := tx.QueryRow(ctx, `SELECT payload_json FROM fallback_decisions WHERE routing_decision_id = ? AND idempotency_key = ?`, routingDecisionID, key).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return FallbackDecision{}, false, nil
	}
	if err != nil {
		return FallbackDecision{}, false, err
	}
	var decision FallbackDecision
	if err := json.Unmarshal([]byte(payload), &decision); err != nil {
		return FallbackDecision{}, false, err
	}
	return decision, true, nil
}

func loadReplanByIdempotency(ctx context.Context, tx storage.Tx, deliveryRunID, key string) (ReplanDecision, bool, error) {
	var payload string
	err := tx.QueryRow(ctx, `SELECT payload_json FROM replan_decisions WHERE delivery_run_id = ? AND idempotency_key = ?`, deliveryRunID, key).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return ReplanDecision{}, false, nil
	}
	if err != nil {
		return ReplanDecision{}, false, err
	}
	var decision ReplanDecision
	if err := json.Unmarshal([]byte(payload), &decision); err != nil {
		return ReplanDecision{}, false, err
	}
	return decision, true, nil
}

func loadFallbackChainTx(ctx context.Context, tx storage.Tx, routingDecisionID string) ([]FallbackDecision, error) {
	rows, err := tx.Query(ctx, `SELECT payload_json FROM fallback_decisions WHERE routing_decision_id = ? ORDER BY fallback_ordinal`, routingDecisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FallbackDecision
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var decision FallbackDecision
		if err := json.Unmarshal([]byte(payload), &decision); err != nil {
			return nil, err
		}
		out = append(out, decision)
	}
	return out, rows.Err()
}

func latestFallbackCandidate(original RoutingDecision, chain []FallbackDecision) string {
	latest := strings.TrimSpace(original.ChosenCandidateID)
	for _, decision := range chain {
		if strings.TrimSpace(decision.SelectedCandidateID) != "" {
			latest = strings.TrimSpace(decision.SelectedCandidateID)
			continue
		}
		if strings.TrimSpace(decision.FallbackCandidateID) != "" {
			latest = strings.TrimSpace(decision.FallbackCandidateID)
		}
	}
	return latest
}

func attemptedFallbackCandidates(original RoutingDecision, chain []FallbackDecision, prior string) []string {
	out := []string{original.ChosenCandidateID, prior}
	for _, decision := range chain {
		out = append(out, decision.PriorCandidateID, decision.FallbackCandidateID, decision.SelectedCandidateID)
	}
	return dedupeStrings(out)
}

func excludeFallbackCandidates(candidates []Candidate, excluded []string) []Candidate {
	blocked := map[string]bool{}
	for _, id := range excluded {
		if strings.TrimSpace(id) != "" {
			blocked[strings.TrimSpace(id)] = true
		}
	}
	out := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if !blocked[candidate.RoutingCandidateID] {
			out = append(out, candidate)
		}
	}
	return out
}

func pinsFromDecisionTx(ctx context.Context, tx storage.Tx, original RoutingDecision, callerPins []Pin) ([]Pin, error) {
	if len(original.UserPinRefs) == 0 {
		return callerPins, nil
	}
	records := make([]PolicyInputRecord, 0, len(original.UserPinRefs))
	for _, id := range original.UserPinRefs {
		record, err := loadPolicyInputTx(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if record.InputKind != PolicyInputKindPin || record.Status != PolicyInputStatusActive ||
			record.ProjectID != original.ProjectID ||
			(record.DeliveryRunID != "" && record.DeliveryRunID != original.DeliveryRunID) ||
			record.RoutingPolicyProfileID != original.RoutingPolicyProfileID ||
			record.PolicyFingerprint != original.PolicyFingerprint ||
			record.ValidationStatus != ValidationStatusValid {
			return nil, &taskrequirements.TypedError{Code: taskrequirements.ErrPinnedCandidateIneligibleCode, Message: "stored user pin does not match original routing decision authority"}
		}
		records = append(records, record)
	}
	pins, _ := constraintsFromPolicyInputRecords(records)
	if len(pins) != len(original.UserPinRefs) {
		return nil, &taskrequirements.TypedError{Code: taskrequirements.ErrPinnedCandidateIneligibleCode, Message: "stored user pin refs could not be recovered"}
	}
	return pins, nil
}

func loadPolicyInputTx(ctx context.Context, tx storage.Tx, id string) (PolicyInputRecord, error) {
	var payload string
	err := tx.QueryRow(ctx, `SELECT payload_json FROM routing_policy_inputs WHERE routing_policy_input_id = ?`, strings.TrimSpace(id)).Scan(&payload)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PolicyInputRecord{}, &taskrequirements.TypedError{Code: taskrequirements.ErrMissingReferenceCode, Message: "routing policy input ref was not persisted"}
		}
		return PolicyInputRecord{}, err
	}
	var record PolicyInputRecord
	if err := json.Unmarshal([]byte(payload), &record); err != nil {
		return PolicyInputRecord{}, err
	}
	return record, nil
}

func isDeliveryRunCancelled(ctx context.Context, store storage.Store, projectID, deliveryRunID string) (bool, error) {
	cancelled := false
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		var err error
		cancelled, err = deliveryRunCancelledTx(ctx, tx, projectID, deliveryRunID)
		return err
	})
	return cancelled, err
}

func deliveryRunCancelledTx(ctx context.Context, tx storage.Tx, projectID, deliveryRunID string) (bool, error) {
	var state string
	err := tx.QueryRow(ctx, `SELECT state FROM delivery_runs WHERE project_id = ? AND delivery_run_id = ?`, projectID, deliveryRunID).Scan(&state)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, &taskrequirements.TypedError{Code: taskrequirements.ErrMissingReferenceCode, Message: "delivery run was not persisted"}
		}
		return false, err
	}
	state = strings.ToLower(strings.TrimSpace(state))
	return state == "cancelled" || state == "canceled" || state == "canceling" || state == "cancelling", nil
}

func releaseHeldReservationsOnCancellation(ctx context.Context, store storage.Store, projectID, deliveryRunID, reservationID string, generation int64, actor delivery.Actor, host delivery.Host) error {
	projectID = strings.TrimSpace(projectID)
	deliveryRunID = strings.TrimSpace(deliveryRunID)
	reservationID = strings.TrimSpace(reservationID)
	if projectID == "" || deliveryRunID == "" {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "project and delivery run are required for cancellation reservation release"}
	}
	var reservations []budget.Reservation
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		run, err := loadReplanRunTx(ctx, tx, projectID, deliveryRunID)
		if err != nil {
			return err
		}
		if reservationID != "" {
			supplied, found, err := loadBudgetReservationTx(ctx, tx, reservationID)
			if err != nil {
				return err
			}
			if !found {
				return &taskrequirements.TypedError{Code: taskrequirements.ErrMissingReferenceCode, Message: "held budget reservation was not persisted"}
			}
			if supplied.Scope.ProjectID != projectID || supplied.Scope.DeliveryRunID != deliveryRunID {
				return fmt.Errorf("%w: supplied reservation does not belong to delivery run", budget.ErrReservationStateConflict)
			}
			if strings.TrimSpace(supplied.AuthorizationFingerprint) != "" && strings.TrimSpace(run.AuthorizationFingerprint) != "" && supplied.AuthorizationFingerprint != run.AuthorizationFingerprint {
				return fmt.Errorf("%w: supplied reservation authorization does not match delivery run", budget.ErrReservationStateConflict)
			}
			if reservationHeld(supplied) && generation != supplied.Generation {
				return fmt.Errorf("%w: supplied reservation generation %d does not match active generation %d", budget.ErrReservationExpired, generation, supplied.Generation)
			}
		}
		rows, err := tx.Query(ctx, `SELECT payload_json FROM budget_reservations
			WHERE project_id = ? AND delivery_run_id = ? AND state IN (?, ?)
			ORDER BY budget_reservation_id`, projectID, deliveryRunID, string(budget.StateActive), string(budget.StatePartiallyCommitted))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				return err
			}
			var reservation budget.Reservation
			if err := json.Unmarshal([]byte(payload), &reservation); err != nil {
				return err
			}
			reservations = append(reservations, reservation)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	for _, reservation := range reservations {
		_, err := budget.Cancel(ctx, store, budget.MutationRequest{
			ReservationID:  reservation.BudgetReservationID,
			IdempotencyKey: "fallback-cancel:" + reservation.BudgetReservationID,
			Generation:     reservation.Generation,
			Actor:          budget.Actor{ActorID: actor.ActorID, Role: actor.DecisionAuthority},
			Host:           budget.Host{HostID: host.HostID, Provider: host.HostKind},
		})
		if err != nil {
			if errors.Is(err, budget.ErrReservationStateConflict) || errors.Is(err, budget.ErrReservationExpired) {
				held, inspectErr := reservationStillHeld(ctx, store, reservation.BudgetReservationID)
				if inspectErr != nil {
					return inspectErr
				}
				if !held {
					continue
				}
			}
			return err
		}
	}
	held, err := activeRunReservations(ctx, store, projectID, deliveryRunID)
	if err != nil {
		return err
	}
	if len(held) > 0 {
		return fmt.Errorf("%w: cancellation left held reservations: %s", budget.ErrReservationStateConflict, strings.Join(held, ","))
	}
	return nil
}

func loadBudgetReservationTx(ctx context.Context, tx storage.Tx, reservationID string) (budget.Reservation, bool, error) {
	var payload string
	err := tx.QueryRow(ctx, `SELECT payload_json FROM budget_reservations WHERE budget_reservation_id = ?`, strings.TrimSpace(reservationID)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return budget.Reservation{}, false, nil
	}
	if err != nil {
		return budget.Reservation{}, false, err
	}
	var reservation budget.Reservation
	if err := json.Unmarshal([]byte(payload), &reservation); err != nil {
		return budget.Reservation{}, false, err
	}
	return reservation, true, nil
}

func activeRunReservations(ctx context.Context, store storage.Store, projectID, deliveryRunID string) ([]string, error) {
	var ids []string
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, `SELECT budget_reservation_id FROM budget_reservations
			WHERE project_id = ? AND delivery_run_id = ? AND state IN (?, ?)
			ORDER BY budget_reservation_id`, projectID, deliveryRunID, string(budget.StateActive), string(budget.StatePartiallyCommitted))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if ids == nil {
		ids = []string{}
	}
	return ids, err
}

func reservationStillHeld(ctx context.Context, store storage.Store, reservationID string) (bool, error) {
	var payload string
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT payload_json FROM budget_reservations WHERE budget_reservation_id = ?`, reservationID).Scan(&payload)
	}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, &taskrequirements.TypedError{Code: taskrequirements.ErrMissingReferenceCode, Message: "held budget reservation was not persisted"}
		}
		return false, err
	}
	var reservation budget.Reservation
	if err := json.Unmarshal([]byte(payload), &reservation); err != nil {
		return false, err
	}
	return reservationHeld(reservation), nil
}

func reservationHeld(reservation budget.Reservation) bool {
	return reservation.State == budget.StateActive || reservation.State == budget.StatePartiallyCommitted
}

type replanRunAuthority struct {
	ProjectID                string
	DeliveryRunID            string
	State                    string
	InputFingerprint         string
	PolicyFingerprint        string
	PlanFingerprint          string
	AuthorizationFingerprint string
	MaxSideEffectClass       string
}

func loadReplanRunTx(ctx context.Context, tx storage.Tx, projectID, deliveryRunID string) (replanRunAuthority, error) {
	var run replanRunAuthority
	err := tx.QueryRow(ctx, `SELECT project_id, delivery_run_id, state, input_fingerprint, policy_fingerprint, plan_fingerprint, authorization_fingerprint, max_side_effect_class
		FROM delivery_runs WHERE project_id = ? AND delivery_run_id = ?`, projectID, deliveryRunID).Scan(
		&run.ProjectID, &run.DeliveryRunID, &run.State, &run.InputFingerprint, &run.PolicyFingerprint, &run.PlanFingerprint, &run.AuthorizationFingerprint, &run.MaxSideEffectClass)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return replanRunAuthority{}, &taskrequirements.TypedError{Code: taskrequirements.ErrMissingReferenceCode, Message: "delivery run was not persisted"}
		}
		return replanRunAuthority{}, err
	}
	return run, nil
}

func sameCanonicalRequirementAuthority(caller, stored taskrequirements.TaskRequirement) bool {
	caller.TaskRequirementFingerprint = stored.TaskRequirementFingerprint
	caller.CreatedAt = stored.CreatedAt
	caller.UpdatedAt = stored.UpdatedAt
	caller.CreatedBy = stored.CreatedBy
	caller.UpdatedBy = stored.UpdatedBy
	caller.Host = stored.Host
	callerPayload, err := delivery.CanonicalJSON(caller)
	if err != nil {
		return false
	}
	storedPayload, err := delivery.CanonicalJSON(stored)
	if err != nil {
		return false
	}
	return string(callerPayload) == string(storedPayload)
}

func validFallbackTrigger(trigger FallbackTrigger) bool {
	switch trigger {
	case FallbackTriggerCandidateFailed, FallbackTriggerBreakerOpened, FallbackTriggerQuotaExhausted, FallbackTriggerRateLimited,
		FallbackTriggerAuthExpired, FallbackTriggerModelRemoved, FallbackTriggerBudgetRefused, FallbackTriggerVerificationFailed,
		FallbackTriggerWorkerFailed, FallbackTriggerTimeout, FallbackTriggerRequirementsChanged, FallbackTriggerUserRequested:
		return true
	default:
		return false
	}
}

func validReplanTrigger(trigger ReplanTrigger) bool {
	switch trigger {
	case ReplanTriggerNoEligibleCandidate, ReplanTriggerLegalFallbackExhausted, ReplanTriggerScopeChangeNeeded,
		ReplanTriggerCapabilityGap, ReplanTriggerGraphBoundHit, ReplanTriggerVerificationFailed,
		ReplanTriggerAmbiguousSideEffectState, ReplanTriggerUserChangedIntent, ReplanTriggerChangedRequirements:
		return true
	default:
		return false
	}
}

func validateSchedulerActor(actor delivery.Actor) error {
	if strings.TrimSpace(actor.ActorKind) == "" || strings.TrimSpace(actor.ActorID) == "" || strings.TrimSpace(actor.DecisionAuthority) == "" || strings.TrimSpace(actor.Source) == "" {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "scheduler decision provenance actor has missing required fields"}
	}
	switch actor.DecisionAuthority {
	case "scheduler", "router", "user":
		return nil
	default:
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "fallback/replan authority must be scheduler, router, or user"}
	}
}

func nonNilAuthorityInputs(values []ChangedAuthorityInput) []ChangedAuthorityInput {
	if values == nil {
		return []ChangedAuthorityInput{}
	}
	return values
}

func markLegality(decision *FallbackDecision, dimension string, legal bool, code taskrequirements.ErrorCode, message string) {
	for i := range decision.LegalityResults {
		if decision.LegalityResults[i].Dimension == dimension {
			decision.LegalityResults[i].Legal = legal
			decision.LegalityResults[i].ErrorCode = code
			decision.LegalityResults[i].Message = message
			return
		}
	}
	decision.LegalityResults = append(decision.LegalityResults, LegalityResult{Dimension: dimension, Legal: legal, ErrorCode: code, Message: message})
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
