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
	if input.Cancelled {
		if err := releaseHeldReservationOnCancellation(ctx, store, input.HeldBudgetReservationID, input.HeldReservationGeneration, input.DecidedBy, input.Host); err != nil {
			return FallbackDecision{}, err
		}
		return FallbackDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "fallback stopped by cancellation before route selection"}
	}
	if err := validateSchedulerActor(input.DecidedBy); err != nil {
		return FallbackDecision{}, err
	}
	if err := validateHost(input.Host); err != nil {
		return FallbackDecision{}, err
	}
	original, err := LoadRoutingDecision(ctx, store, input.RoutingDecisionID)
	if err != nil {
		return FallbackDecision{}, err
	}
	profile, err := resolveFallbackProfile(ctx, store, original)
	if err != nil {
		return FallbackDecision{}, err
	}
	if err := validateFallbackInputs(original, profile, input.Inputs); err != nil {
		return FallbackDecision{}, err
	}
	if isDeliveryRunCancelled(ctx, store, original.ProjectID, original.DeliveryRunID) {
		if err := releaseHeldReservationOnCancellation(ctx, store, input.HeldBudgetReservationID, input.HeldReservationGeneration, input.DecidedBy, input.Host); err != nil {
			return FallbackDecision{}, err
		}
		return FallbackDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "fallback stopped by durable cancellation before route selection"}
	}
	evaluated := input.Inputs
	evaluated.Policy = profile.EligibilityPolicy
	evaluated.Pins = pinsFromDecision(original, evaluated.Pins)
	evaluated.Candidates = excludeFallbackCandidates(evaluated.Candidates, append(original.FallbackChain, input.PriorCandidateID))
	eligibility := FilterHardEligibility(evaluated)
	scored := scoreCandidates(eligibility.Eligible, evaluated, profile.OptimizationPolicy)
	selected := Candidate{}
	if len(scored) > 0 {
		selected = scored[0].Candidate
	}
	fingerprint, err := fallbackRoutingFingerprint(original, profile, input, eligibility, selected)
	if err != nil {
		return FallbackDecision{}, err
	}
	idempotencyKey := fallbackIdempotencyKey(input, fingerprint)
	var stored FallbackDecision
	err = store.WithWriteTx(ctx, func(tx storage.Tx) error {
		if deliveryRunCancelledTx(ctx, tx, original.ProjectID, original.DeliveryRunID) {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "fallback stopped by durable cancellation at transaction boundary"}
		}
		if existing, ok, err := loadFallbackByIdempotency(ctx, tx, original.RoutingDecisionID, idempotencyKey); err != nil || ok {
			if ok && (existing.Trigger != input.Trigger || existing.PriorCandidateID != strings.TrimSpace(input.PriorCandidateID) || existing.RoutingFingerprint != fingerprint) {
				return &delivery.TypedError{Code: delivery.ErrDuplicateReplayCode, Message: "fallback idempotency key replayed with different request"}
			}
			stored = existing
			return err
		}
		used, err := countFallbacksTx(ctx, tx, original.RoutingDecisionID)
		if err != nil {
			return err
		}
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
		if errors.Is(err, taskrequirements.ErrReplanRequired) {
			_ = releaseHeldReservationOnCancellation(ctx, store, input.HeldBudgetReservationID, input.HeldReservationGeneration, input.DecidedBy, input.Host)
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
	if input.Cancelled {
		if err := releaseHeldReservationOnCancellation(ctx, store, input.HeldBudgetReservationID, input.HeldReservationGeneration, input.DecidedBy, input.Host); err != nil {
			return ReplanDecision{}, err
		}
		return ReplanDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "replan stopped by cancellation before planning"}
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
	profile, err := resolveReplanProfile(ctx, store, input.RoutingPolicyProfileID)
	if err != nil {
		return ReplanDecision{}, err
	}
	idempotencyKey := replanIdempotencyKey(input)
	var stored ReplanDecision
	err = store.WithWriteTx(ctx, func(tx storage.Tx) error {
		if deliveryRunCancelledTx(ctx, tx, input.ProjectID, input.DeliveryRunID) {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "replan stopped by durable cancellation at transaction boundary"}
		}
		if existing, ok, err := loadReplanByIdempotency(ctx, tx, input.DeliveryRunID, idempotencyKey); err != nil || ok {
			if ok && (existing.Trigger != input.Trigger || existing.PriorPlanFingerprint != input.PriorPlanFingerprint || existing.NewPlanFingerprint != strings.TrimSpace(input.NewPlanFingerprint) || existing.RoutingFingerprint != strings.TrimSpace(input.RoutingFingerprint)) {
				return &delivery.TypedError{Code: delivery.ErrDuplicateReplayCode, Message: "replan idempotency key replayed with different request"}
			}
			stored = existing
			return err
		}
		used, err := countReplansTx(ctx, tx, input.ProjectID, input.DeliveryRunID)
		if err != nil {
			return err
		}
		decision := buildReplanDecision(profile, input, idempotencyKey, used+1, store.Now())
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
		if errors.Is(err, taskrequirements.ErrReplanRequired) {
			_ = releaseHeldReservationOnCancellation(ctx, store, input.HeldBudgetReservationID, input.HeldReservationGeneration, input.DecidedBy, input.Host)
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

func resolveFallbackProfile(ctx context.Context, store storage.Store, original RoutingDecision) (RoutingPolicyProfile, error) {
	if original.RoutingPolicyProfile != nil {
		profile := normalizeRoutingPolicyProfile(*original.RoutingPolicyProfile)
		if err := ValidateRoutingPolicyProfile(profile); err != nil {
			return RoutingPolicyProfile{}, err
		}
		return profile, nil
	}
	return LoadRoutingPolicyProfile(ctx, store, original.RoutingPolicyProfileID)
}

func resolveReplanProfile(ctx context.Context, store storage.Store, profileID string) (RoutingPolicyProfile, error) {
	if strings.TrimSpace(profileID) == "" {
		profileID = defaultStoredRoutingPolicyProfileID(store.Now())
	}
	return LoadRoutingPolicyProfile(ctx, store, profileID)
}

func validateFallbackInputs(original RoutingDecision, profile RoutingPolicyProfile, inputs Inputs) error {
	if original.DecisionStatus != DecisionStatusSelected || original.ChosenCandidateID == "" {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "fallback requires an original selected route"}
	}
	if inputs.Requirement.ProjectID != original.ProjectID || inputs.Requirement.DeliveryRunID != original.DeliveryRunID ||
		inputs.Requirement.TaskID != original.TaskID || inputs.Requirement.TaskRequirementID != original.TaskRequirementID {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "fallback requirement does not match original route"}
	}
	if inputs.Requirement.PlanFingerprint != original.PlanFingerprint {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "fallback cannot change plan fingerprint"}
	}
	if profile.PolicyFingerprint != original.PolicyFingerprint {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "fallback profile fingerprint does not match original route"}
	}
	return nil
}

func fallbackRoutingFingerprint(original RoutingDecision, profile RoutingPolicyProfile, input FallbackInput, eligibility Result, selected Candidate) (string, error) {
	digest, _, err := delivery.DigestCanonicalJSON(map[string]any{
		"routing_decision_id":   original.RoutingDecisionID,
		"original_fingerprint":  original.RoutingFingerprint,
		"policy_fingerprint":    profile.PolicyFingerprint,
		"trigger":               input.Trigger,
		"prior_candidate_id":    input.PriorCandidateID,
		"selected_candidate_id": selected.RoutingCandidateID,
		"eligible_candidates":   eligibility.Eligible,
		"rejected_candidates":   eligibility.Rejected,
		"changed_authority":     input.ChangedAuthorityInputs,
	})
	return digest, err
}

func fallbackIdempotencyKey(input FallbackInput, fingerprint string) string {
	if strings.TrimSpace(input.IdempotencyKey) != "" {
		return strings.TrimSpace(input.IdempotencyKey)
	}
	return "fallback:" + string(input.Trigger) + ":" + strings.TrimSpace(input.PriorCandidateID) + ":" + fingerprint
}

func replanIdempotencyKey(input ReplanInput) string {
	if strings.TrimSpace(input.IdempotencyKey) != "" {
		return strings.TrimSpace(input.IdempotencyKey)
	}
	fp := input.NewPlanFingerprint
	if fp == "" {
		fp = input.RoutingFingerprint
	}
	return "replan:" + string(input.Trigger) + ":" + input.PriorPlanFingerprint + ":" + fp
}

func fallbackDecisionID(routingDecisionID string, ordinal int, routingFingerprint string) string {
	return "fdec_" + digestBase32(routingDecisionID, fmt.Sprintf("%d", ordinal), routingFingerprint)
}

func replanDecisionID(projectID, deliveryRunID, priorPlanFingerprint string, ordinal int) string {
	return "replan_" + digestBase32(projectID, deliveryRunID, priorPlanFingerprint, fmt.Sprintf("%d", ordinal))
}

func countFallbacksTx(ctx context.Context, tx storage.Tx, routingDecisionID string) (int, error) {
	var count int
	err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM fallback_decisions WHERE routing_decision_id = ?`, routingDecisionID).Scan(&count)
	return count, err
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

func pinsFromDecision(original RoutingDecision, pins []Pin) []Pin {
	if len(pins) > 0 || len(original.UserPinRefs) == 0 {
		return pins
	}
	return pins
}

func isDeliveryRunCancelled(ctx context.Context, store storage.Store, projectID, deliveryRunID string) bool {
	cancelled := false
	_ = store.WithTx(ctx, func(tx storage.Tx) error {
		cancelled = deliveryRunCancelledTx(ctx, tx, projectID, deliveryRunID)
		return nil
	})
	return cancelled
}

func deliveryRunCancelledTx(ctx context.Context, tx storage.Tx, projectID, deliveryRunID string) bool {
	var state string
	err := tx.QueryRow(ctx, `SELECT state FROM delivery_runs WHERE project_id = ? AND delivery_run_id = ?`, projectID, deliveryRunID).Scan(&state)
	if err != nil {
		return false
	}
	state = strings.ToLower(strings.TrimSpace(state))
	return state == "cancelled" || state == "canceled" || state == "canceling" || state == "cancelling"
}

func releaseHeldReservationOnCancellation(ctx context.Context, store storage.Store, reservationID string, generation int64, actor delivery.Actor, host delivery.Host) error {
	reservationID = strings.TrimSpace(reservationID)
	if reservationID == "" {
		return nil
	}
	if generation <= 0 {
		return fmt.Errorf("%w: held reservation generation is required for cancellation", budget.ErrReservationStateConflict)
	}
	_, err := budget.Cancel(ctx, store, budget.MutationRequest{
		ReservationID:  reservationID,
		IdempotencyKey: "fallback-cancel:" + reservationID,
		Generation:     generation,
		Actor:          budget.Actor{ActorID: actor.ActorID, Role: actor.DecisionAuthority},
		Host:           budget.Host{HostID: host.HostID, Provider: host.HostKind, Model: host.LoopcoderVersion},
	})
	if errors.Is(err, budget.ErrReservationStateConflict) || errors.Is(err, budget.ErrReservationExpired) {
		return nil
	}
	return err
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
