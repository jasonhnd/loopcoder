package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

const (
	VerificationDecisionSchema = "loopcoder.verification_decision.v1"

	VerificationStatusAccepted   = "accepted"
	VerificationStatusRejected   = "rejected"
	VerificationStatusNeedsHuman = "needs-human"

	VerificationVerdictPass       = "pass"
	VerificationVerdictFail       = "fail"
	VerificationVerdictNeedsHuman = "needs-human"

	FinalAuthorityAutomatedVerifier = "automated-verifier"
	FinalAuthorityCouncil           = "bounded-council"
	FinalAuthorityHuman             = "human"
)

const (
	maxCouncilDurationMS = int64(1<<63-1) / int64(time.Millisecond)
	maxAuthorityKeyBytes = 128
)

type EvidenceRef struct {
	RecordKind string `json:"record_kind"`
	RecordID   string `json:"record_id"`
	Summary    string `json:"summary,omitempty"`
}

type VerifierVerdict struct {
	MemberID              string                     `json:"member_id"`
	RoutingCandidateID    string                     `json:"routing_candidate_id"`
	Verdict               string                     `json:"verdict"`
	EvidenceRefs          []EvidenceRef              `json:"evidence_refs"`
	Message               string                     `json:"message,omitempty"`
	Authority             string                     `json:"authority"`
	AuthorityFingerprint  string                     `json:"authority_fingerprint"`
	VerificationStartedAt string                     `json:"verification_started_at,omitempty"`
	VerificationEndedAt   string                     `json:"verification_ended_at,omitempty"`
	TerminalErrorCode     taskrequirements.ErrorCode `json:"terminal_error_code,omitempty"`
}

type CouncilLimits struct {
	Enabled         bool   `json:"enabled"`
	MaxMembers      int    `json:"max_members"`
	MaxRounds       int    `json:"max_rounds"`
	MaxDurationMS   int64  `json:"max_duration_ms"`
	MaxBudgetTokens int64  `json:"max_budget_tokens"`
	StartedAt       string `json:"started_at,omitempty"`
	DeadlineAt      string `json:"deadline_at,omitempty"`
}

type CouncilState struct {
	Enabled           bool                       `json:"enabled"`
	MaxMembers        int                        `json:"max_members,omitempty"`
	MaxRounds         int                        `json:"max_rounds,omitempty"`
	MaxDurationMS     int64                      `json:"max_duration_ms,omitempty"`
	MaxBudgetTokens   int64                      `json:"max_budget_tokens,omitempty"`
	MemberCount       int                        `json:"member_count"`
	RoundsUsed        int                        `json:"rounds_used"`
	BudgetTokensUsed  int64                      `json:"budget_tokens_used"`
	TimedOut          bool                       `json:"timed_out"`
	BoundExceeded     string                     `json:"bound_exceeded,omitempty"`
	Outcome           string                     `json:"outcome"`
	TerminalErrorCode taskrequirements.ErrorCode `json:"terminal_error_code,omitempty"`
}

type CouncilMemberAuthority struct {
	MemberID             string                             `json:"member_id,omitempty"`
	RoutingDecisionID    string                             `json:"routing_decision_id"`
	RoutingCandidateID   string                             `json:"routing_candidate_id"`
	RoutingFingerprint   string                             `json:"routing_fingerprint"`
	CandidateFingerprint string                             `json:"candidate_fingerprint"`
	ActualIndependence   taskrequirements.IndependenceLevel `json:"actual_independence"`
}

type VerificationDecision struct {
	SchemaVersion             string                             `json:"schema_version"`
	RecordVersion             int                                `json:"record_version"`
	VerificationDecisionID    string                             `json:"verification_decision_id"`
	ProjectID                 string                             `json:"project_id"`
	DeliveryRunID             string                             `json:"delivery_run_id"`
	TaskID                    string                             `json:"task_id"`
	TaskRequirementID         string                             `json:"task_requirement_id"`
	WorkerRoutingDecisionID   string                             `json:"worker_routing_decision_id"`
	VerifierRoutingDecisionID string                             `json:"verifier_routing_decision_id,omitempty"`
	DecisionKey               string                             `json:"decision_key"`
	IdempotencyKey            string                             `json:"idempotency_key"`
	DecisionStatus            string                             `json:"decision_status"`
	Verdict                   string                             `json:"verdict"`
	RequiredIndependence      taskrequirements.IndependenceLevel `json:"required_independence"`
	ActualIndependence        taskrequirements.IndependenceLevel `json:"actual_independence"`
	EligibilityExclusions     []RejectionReason                  `json:"eligibility_exclusions"`
	WorkerCandidateID         string                             `json:"worker_candidate_id"`
	VerifierCandidateID       string                             `json:"verifier_candidate_id,omitempty"`
	VerifierVerdicts          []VerifierVerdict                  `json:"verifier_verdicts"`
	Disagreements             []string                           `json:"disagreements"`
	EvidenceRefs              []EvidenceRef                      `json:"evidence_refs"`
	Council                   CouncilState                       `json:"council"`
	CouncilMemberAuthorities  []CouncilMemberAuthority           `json:"council_member_authorities,omitempty"`
	Timeout                   bool                               `json:"timeout"`
	FinalAuthority            string                             `json:"final_authority"`
	FinalAuthorityFingerprint string                             `json:"final_authority_fingerprint"`
	VerificationFingerprint   string                             `json:"verification_fingerprint"`
	PolicyFingerprint         string                             `json:"policy_fingerprint"`
	PlanFingerprint           string                             `json:"plan_fingerprint"`
	CreatedAt                 string                             `json:"created_at"`
	UpdatedAt                 string                             `json:"updated_at"`
	DecidedBy                 delivery.Actor                     `json:"decided_by"`
	Host                      delivery.Host                      `json:"host"`
	TerminalErrorCode         taskrequirements.ErrorCode         `json:"terminal_error_code,omitempty"`
}

type VerificationDecisionInput struct {
	WorkerRoutingDecisionID         string
	VerifierRoutingDecisionID       string
	CouncilMemberRoutingDecisionIDs []string
	DecisionKey                     string
	IdempotencyKey                  string
	VerifierVerdicts                []VerifierVerdict
	CouncilLimits                   CouncilLimits
	CouncilRoundsUsed               int
	CouncilBudgetTokensUsed         int64
	Timeout                         bool
	AuthorityFingerprint            string
	DecidedBy                       delivery.Actor
	Host                            delivery.Host
}

func DecideAndPersistVerification(ctx context.Context, store storage.Store, input VerificationDecisionInput) (VerificationDecision, error) {
	if store == nil {
		return VerificationDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "store is required"}
	}
	if strings.TrimSpace(input.WorkerRoutingDecisionID) == "" {
		return VerificationDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "worker route and decision key are required"}
	}
	if err := validateAuthorityKey("decision key", input.DecisionKey, true); err != nil {
		return VerificationDecision{}, err
	}
	if err := validateAuthorityKey("idempotency key", input.IdempotencyKey, false); err != nil {
		return VerificationDecision{}, err
	}
	if err := validateSchedulerActor(input.DecidedBy); err != nil {
		return VerificationDecision{}, err
	}
	if err := validateHost(input.Host); err != nil {
		return VerificationDecision{}, err
	}
	var stored VerificationDecision
	err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		workerRoute, err := loadRoutingDecisionTx(ctx, tx, input.WorkerRoutingDecisionID)
		if err != nil {
			return err
		}
		req, err := loadTaskRequirementTx(ctx, tx, workerRoute.TaskRequirementID)
		if err != nil {
			return err
		}
		profile, err := loadRoutingPolicyProfileTx(ctx, tx, workerRoute.RoutingPolicyProfileID)
		if err != nil {
			return err
		}
		verifierRoute, verifierCandidate, verifierReq, exclusions, err := resolveVerifierRouteTx(ctx, tx, input.VerifierRoutingDecisionID)
		if err != nil {
			return err
		}
		if verifierRoute.RoutingDecisionID != "" && (verifierRoute.ProjectID != workerRoute.ProjectID || verifierRoute.DeliveryRunID != workerRoute.DeliveryRunID) {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrCrossProjectReferenceCode, Message: "verifier route does not belong to worker delivery run"}
		}
		if err := requirePrimaryVerifierPlanAuthority(workerRoute, verifierRoute, verifierReq); err != nil {
			return err
		}
		workerCandidate, ok := chosenCandidate(workerRoute)
		if !ok {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "worker route has no selected candidate to verify"}
		}
		required := RequiredVerifierIndependence(req, profile)
		memberAuthorities, authorityDisagreements, authorityTerminal, err := resolveCouncilMemberAuthoritiesTx(ctx, tx, input, workerRoute, workerCandidate, verifierRoute, verifierCandidate, verifierReq, required)
		if err != nil {
			return err
		}
		decision, err := buildVerificationDecision(workerRoute, verifierRoute, req, verifierReq, profile, workerCandidate, verifierCandidate, exclusions, memberAuthorities, authorityDisagreements, authorityTerminal, input, store.Now())
		if err != nil {
			return err
		}
		if existing, ok, err := loadVerificationByIdempotency(ctx, tx, workerRoute.DeliveryRunID, workerRoute.TaskID, decision.DecisionKey, decision.IdempotencyKey); err != nil || ok {
			if err != nil {
				return err
			}
			if existing.VerificationFingerprint != decision.VerificationFingerprint {
				return &delivery.TypedError{Code: delivery.ErrDuplicateReplayCode, Message: "verification idempotency key replayed with different request"}
			}
			stored = existing
			return nil
		}
		if existing, ok, err := loadVerificationByAuthority(ctx, tx, decision.DeliveryRunID, decision.TaskID, decision.DecisionKey, decision.FinalAuthorityFingerprint); err != nil || ok {
			if err != nil {
				return err
			}
			if existing.VerificationFingerprint != decision.VerificationFingerprint {
				return taskrequirements.ErrVerificationDecisionConflict
			}
			stored = existing
			return nil
		}
		if err := insertVerificationDecisionTx(ctx, tx, decision); err != nil {
			return err
		}
		stored = decision
		return nil
	})
	if err != nil {
		return VerificationDecision{}, err
	}
	if stored.TerminalErrorCode != "" {
		return stored, verificationTerminalError(stored.TerminalErrorCode)
	}
	return stored, nil
}

func RequiredVerifierIndependence(req taskrequirements.TaskRequirement, profile RoutingPolicyProfile) taskrequirements.IndependenceLevel {
	required := profile.EligibilityPolicy.VerifierIndependence
	if required == "" {
		required = taskrequirements.IndependenceNone
	}
	if risk, ok := profile.RiskPolicy[req.RiskTier]; ok {
		required = strongerIndependence(required, risk.IndependenceLevel)
	}
	if level, ok := profile.VerifierSeparation[req.RiskTier]; ok {
		required = strongerIndependence(required, level)
	}
	for _, verification := range req.VerificationRequirements {
		if riskApplies(req.RiskTier, verification.RequiredForRiskTiers) {
			required = strongerIndependence(required, verification.IndependenceLevel)
		}
	}
	return required
}

func requirePrimaryVerifierPlanAuthority(workerRoute, verifierRoute RoutingDecision, verifierReq taskrequirements.TaskRequirement) error {
	if verifierRoute.RoutingDecisionID == "" {
		return nil
	}
	if verifierRoute.PlanFingerprint != workerRoute.PlanFingerprint {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "verifier route does not match worker plan authority"}
	}
	if verifierReq.TaskRequirementID != "" && verifierReq.PlanFingerprint != workerRoute.PlanFingerprint {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "verifier task requirement does not match worker plan authority"}
	}
	return nil
}

func buildVerificationDecision(workerRoute, verifierRoute RoutingDecision, req, verifierReq taskrequirements.TaskRequirement, profile RoutingPolicyProfile, worker, verifier Candidate, exclusions []RejectionReason, memberAuthorities []CouncilMemberAuthority, authorityDisagreements []string, authorityTerminal taskrequirements.ErrorCode, input VerificationDecisionInput, now time.Time) (VerificationDecision, error) {
	required := RequiredVerifierIndependence(req, profile)
	actual := actualIndependence(worker, verifier)
	verdicts := sanitizeVerifierVerdicts(input.VerifierVerdicts)
	verdictValidation := validateBoundVerifierVerdicts(input.VerifierVerdicts, verifierRoute, verifier, memberAuthorities, input.CouncilLimits.Enabled)
	council := aggregateCouncil(input.CouncilLimits, input.CouncilRoundsUsed, input.CouncilBudgetTokensUsed, input.Timeout, verdictValidation.memberCount, verdicts, now)
	evidence := evidenceRefs(verdicts)
	disagreements := councilDisagreements(verdicts, council)
	disagreements = append(disagreements, verdictValidation.disagreements...)
	disagreements = append(disagreements, authorityDisagreements...)
	status := VerificationStatusAccepted
	verdict := VerificationVerdictPass
	authority := FinalAuthorityAutomatedVerifier
	terminal := taskrequirements.ErrorCode("")

	if input.CouncilLimits.Enabled {
		authority = FinalAuthorityCouncil
	}
	if required == taskrequirements.IndependenceHuman {
		status, verdict, authority, terminal = VerificationStatusNeedsHuman, VerificationVerdictNeedsHuman, FinalAuthorityHuman, taskrequirements.ErrVerifierIndependenceRequiredCode
		disagreements = append(disagreements, "human independence is required by policy")
	} else if verifier.RoutingCandidateID == "" || verifierRoute.DecisionStatus != DecisionStatusSelected {
		status, verdict, authority, terminal = VerificationStatusNeedsHuman, VerificationVerdictNeedsHuman, FinalAuthorityHuman, taskrequirements.ErrNoEligibleCandidateCode
	} else if verifier.Permission != taskrequirements.PermissionReadOnly {
		status, verdict, authority, terminal = VerificationStatusNeedsHuman, VerificationVerdictNeedsHuman, FinalAuthorityHuman, taskrequirements.ErrCapabilityUnsupportedCode
		disagreements = append(disagreements, "verifier route is not read-only")
	} else if !verifierRouteAllowed(verifierReq, verifier) {
		status, verdict, authority, terminal = VerificationStatusNeedsHuman, VerificationVerdictNeedsHuman, FinalAuthorityHuman, taskrequirements.ErrCapabilityUnsupportedCode
		disagreements = append(disagreements, "selected route is not a read-only verifier route with verification output")
	} else if !workerVerificationOutputAllowed(req, profile) {
		status, verdict, authority, terminal = VerificationStatusNeedsHuman, VerificationVerdictNeedsHuman, FinalAuthorityHuman, taskrequirements.ErrCapabilityUnsupportedCode
		disagreements = append(disagreements, "worker verification output contract is not verification-verdict or json-schema")
	} else if !independentEnough(worker, verifier, required) {
		status, verdict, authority, terminal = VerificationStatusNeedsHuman, VerificationVerdictNeedsHuman, FinalAuthorityHuman, taskrequirements.ErrVerifierIndependenceRequiredCode
		disagreements = append(disagreements, "verifier route is not independent enough from worker route")
	} else if authorityTerminal != "" {
		status, verdict, authority, terminal = VerificationStatusNeedsHuman, VerificationVerdictNeedsHuman, FinalAuthorityHuman, authorityTerminal
	} else if verdictValidation.terminal != "" {
		status, verdict, authority, terminal = VerificationStatusNeedsHuman, VerificationVerdictNeedsHuman, FinalAuthorityHuman, verdictValidation.terminal
	} else if council.TerminalErrorCode != "" {
		status, verdict, authority, terminal = VerificationStatusNeedsHuman, VerificationVerdictNeedsHuman, FinalAuthorityHuman, council.TerminalErrorCode
	} else if len(verdicts) == 0 {
		status, verdict, authority, terminal = VerificationStatusNeedsHuman, VerificationVerdictNeedsHuman, FinalAuthorityHuman, taskrequirements.ErrRequirementConfidenceInsufficientCode
		disagreements = append(disagreements, "no verifier verdict was returned")
	} else if council.Outcome == VerificationVerdictFail {
		status, verdict, terminal = VerificationStatusRejected, VerificationVerdictFail, taskrequirements.ErrVerificationDecisionConflictCode
	} else if council.Outcome == VerificationVerdictNeedsHuman {
		status, verdict, authority, terminal = VerificationStatusNeedsHuman, VerificationVerdictNeedsHuman, FinalAuthorityHuman, taskrequirements.ErrVerificationDecisionConflictCode
	} else {
		terminal = ""
	}

	authorityFP := strings.TrimSpace(input.AuthorityFingerprint)
	if authorityFP == "" {
		authorityFP = workerRoute.RoutingFingerprint
	}
	if !validFingerprint(authorityFP) {
		return VerificationDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "verification authority fingerprint must be a sha256 digest"}
	}
	input.IdempotencyKey = verificationIdempotencyKey(input, workerRoute, verifierRoute, memberAuthorities)
	if err := validateAuthorityKey("idempotency key", input.IdempotencyKey, true); err != nil {
		return VerificationDecision{}, err
	}
	fp := verificationRequestFingerprint(input, workerRoute, verifierRoute, memberAuthorities)
	createdAt := delivery.CanonicalTimestamp(now)
	decision := VerificationDecision{
		SchemaVersion:             VerificationDecisionSchema,
		RecordVersion:             1,
		ProjectID:                 workerRoute.ProjectID,
		DeliveryRunID:             workerRoute.DeliveryRunID,
		TaskID:                    workerRoute.TaskID,
		TaskRequirementID:         req.TaskRequirementID,
		WorkerRoutingDecisionID:   workerRoute.RoutingDecisionID,
		VerifierRoutingDecisionID: verifierRoute.RoutingDecisionID,
		DecisionKey:               strings.TrimSpace(input.DecisionKey),
		IdempotencyKey:            strings.TrimSpace(input.IdempotencyKey),
		DecisionStatus:            status,
		Verdict:                   verdict,
		RequiredIndependence:      required,
		ActualIndependence:        actual,
		EligibilityExclusions:     nonNilRejectionReasons(exclusions),
		WorkerCandidateID:         worker.RoutingCandidateID,
		VerifierCandidateID:       verifier.RoutingCandidateID,
		VerifierVerdicts:          verdicts,
		Disagreements:             nonNilStrings(dedupeStrings(disagreements)),
		EvidenceRefs:              evidence,
		Council:                   council,
		CouncilMemberAuthorities:  memberAuthorities,
		Timeout:                   input.Timeout || council.TimedOut,
		FinalAuthority:            authority,
		FinalAuthorityFingerprint: authorityFP,
		VerificationFingerprint:   fp,
		PolicyFingerprint:         profile.PolicyFingerprint,
		PlanFingerprint:           workerRoute.PlanFingerprint,
		CreatedAt:                 createdAt,
		UpdatedAt:                 createdAt,
		DecidedBy:                 input.DecidedBy,
		Host:                      input.Host,
		TerminalErrorCode:         terminal,
	}
	decision.VerificationDecisionID = verificationDecisionID(decision.ProjectID, decision.DeliveryRunID, decision.TaskID, decision.DecisionKey, decision.FinalAuthorityFingerprint, decision.VerificationFingerprint)
	if err := validateVerificationDecision(decision); err != nil {
		return VerificationDecision{}, err
	}
	return decision, nil
}

type verdictValidationResult struct {
	memberCount   int
	terminal      taskrequirements.ErrorCode
	disagreements []string
}

func validateBoundVerifierVerdicts(verdicts []VerifierVerdict, verifierRoute RoutingDecision, verifier Candidate, authorities []CouncilMemberAuthority, councilEnabled bool) verdictValidationResult {
	result := verdictValidationResult{}
	if councilEnabled {
		result.memberCount = len(authorities)
	} else {
		result.memberCount = len(verdicts)
	}
	if verifierRoute.DecisionStatus != DecisionStatusSelected || verifier.RoutingCandidateID == "" {
		return result
	}
	if !councilEnabled && len(verdicts) != 1 {
		result.terminal = taskrequirements.ErrVerificationDecisionConflictCode
		result.disagreements = append(result.disagreements, "non-council verification requires exactly one verifier verdict")
		return result
	}
	if councilEnabled && len(verdicts) != len(authorities) {
		result.terminal = taskrequirements.ErrVerificationDecisionConflictCode
		result.disagreements = append(result.disagreements, "council verifier verdicts do not match explicit member authorities")
	}
	authorityByFingerprint := map[string]CouncilMemberAuthority{}
	for _, authority := range authorities {
		authorityByFingerprint[authority.RoutingFingerprint] = authority
	}
	seenAuthorities := map[string]struct{}{}
	for _, verdict := range verdicts {
		memberID := strings.TrimSpace(verdict.MemberID)
		if memberID != "" && !validVerifierIdentity(memberID) {
			result.disagreements = append(result.disagreements, "verifier member identity is missing or invalid")
			result.terminal = taskrequirements.ErrVerificationDecisionConflictCode
		}
		verdictCandidateID := strings.TrimSpace(verdict.RoutingCandidateID)
		verdictFingerprint := strings.TrimSpace(verdict.AuthorityFingerprint)
		if councilEnabled {
			authority, ok := authorityByFingerprint[verdictFingerprint]
			if !ok || authority.RoutingCandidateID != verdictCandidateID {
				result.disagreements = append(result.disagreements, "verifier verdict is not bound to an explicit council member authority")
				result.terminal = taskrequirements.ErrVerificationDecisionConflictCode
			} else {
				if _, duplicate := seenAuthorities[verdictFingerprint]; duplicate {
					result.disagreements = append(result.disagreements, "duplicate verifier verdict authority")
					result.terminal = taskrequirements.ErrVerificationDecisionConflictCode
				}
				seenAuthorities[verdictFingerprint] = struct{}{}
			}
		} else if verdictCandidateID != verifierRoute.ChosenCandidateID || verdictCandidateID != verifier.RoutingCandidateID {
			result.disagreements = append(result.disagreements, "verifier verdict is not bound to selected verifier candidate")
			result.terminal = taskrequirements.ErrVerificationDecisionConflictCode
		}
		if !validFingerprint(verdictFingerprint) || (!councilEnabled && verdictFingerprint != verifierRoute.RoutingFingerprint) {
			result.disagreements = append(result.disagreements, "verifier verdict authority fingerprint does not match selected verifier route")
			result.terminal = taskrequirements.ErrVerificationDecisionConflictCode
		}
		if !allowedVerifierVerdictAuthority(verdict.Authority) {
			result.disagreements = append(result.disagreements, "verifier verdict claimed forbidden authority")
			result.terminal = taskrequirements.ErrVerificationDecisionConflictCode
		}
		switch verdict.Verdict {
		case VerificationVerdictPass, VerificationVerdictFail, VerificationVerdictNeedsHuman:
		default:
			result.disagreements = append(result.disagreements, "verifier verdict enum is unknown")
			result.terminal = taskrequirements.ErrVerificationDecisionConflictCode
		}
		if verdict.TerminalErrorCode != "" {
			result.disagreements = append(result.disagreements, "verifier verdict carried terminal error")
			result.terminal = taskrequirements.ErrVerificationDecisionConflictCode
		}
	}
	if councilEnabled && len(seenAuthorities) != len(authorities) {
		result.disagreements = append(result.disagreements, "council member authority is missing a verifier verdict")
		result.terminal = taskrequirements.ErrVerificationDecisionConflictCode
	}
	return result
}

func resolveCouncilMemberAuthoritiesTx(ctx context.Context, tx storage.Tx, input VerificationDecisionInput, workerRoute RoutingDecision, worker Candidate, verifierRoute RoutingDecision, verifier Candidate, verifierReq taskrequirements.TaskRequirement, required taskrequirements.IndependenceLevel) ([]CouncilMemberAuthority, []string, taskrequirements.ErrorCode, error) {
	ids := explicitCouncilMemberRouteIDs(input, verifierRoute)
	if len(ids) == 0 {
		return nil, nil, "", nil
	}
	authorities := make([]CouncilMemberAuthority, 0, len(ids))
	disagreements := []string{}
	terminal := taskrequirements.ErrorCode("")
	seenRoute, seenCandidate, seenFingerprint := map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}
	for _, id := range ids {
		route, candidate, req, _, err := resolveVerifierRouteTx(ctx, tx, id)
		if err != nil {
			return nil, nil, "", err
		}
		if route.ProjectID != workerRoute.ProjectID || route.DeliveryRunID != workerRoute.DeliveryRunID {
			return nil, nil, "", &taskrequirements.TypedError{Code: taskrequirements.ErrCrossProjectReferenceCode, Message: "council verifier route does not belong to worker delivery run"}
		}
		if route.PlanFingerprint != workerRoute.PlanFingerprint {
			return nil, nil, "", &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "council verifier route does not match worker plan authority"}
		}
		duplicateRoute := false
		if _, ok := seenRoute[route.RoutingDecisionID]; ok {
			disagreements = append(disagreements, "duplicate council verifier route")
			terminal = taskrequirements.ErrVerificationDecisionConflictCode
			duplicateRoute = true
		}
		seenRoute[route.RoutingDecisionID] = struct{}{}
		if _, ok := seenCandidate[candidate.RoutingCandidateID]; candidate.RoutingCandidateID != "" && ok {
			disagreements = append(disagreements, "duplicate council verifier candidate")
			terminal = taskrequirements.ErrVerificationDecisionConflictCode
		}
		if candidate.RoutingCandidateID != "" {
			seenCandidate[candidate.RoutingCandidateID] = struct{}{}
		}
		if _, ok := seenFingerprint[route.RoutingFingerprint]; route.RoutingFingerprint != "" && ok {
			disagreements = append(disagreements, "duplicate council verifier authority fingerprint")
			terminal = taskrequirements.ErrVerificationDecisionConflictCode
		}
		if route.RoutingFingerprint != "" {
			seenFingerprint[route.RoutingFingerprint] = struct{}{}
		}
		switch {
		case route.DecisionStatus != DecisionStatusSelected || candidate.RoutingCandidateID == "":
			disagreements = append(disagreements, "council verifier route has no selected candidate")
			terminal = taskrequirements.ErrNoEligibleCandidateCode
		case candidate.Permission != taskrequirements.PermissionReadOnly:
			disagreements = append(disagreements, "council verifier route is not read-only")
			terminal = taskrequirements.ErrCapabilityUnsupportedCode
		case !verifierRouteAllowed(req, candidate):
			disagreements = append(disagreements, "council verifier route is not a read-only verifier route with verification output")
			terminal = taskrequirements.ErrCapabilityUnsupportedCode
		case !independentEnough(worker, candidate, required):
			disagreements = append(disagreements, "council verifier route is not independent enough from worker route")
			terminal = taskrequirements.ErrVerifierIndependenceRequiredCode
		}
		if duplicateRoute || route.DecisionStatus != DecisionStatusSelected || candidate.RoutingCandidateID == "" {
			continue
		}
		authorities = append(authorities, CouncilMemberAuthority{
			RoutingDecisionID:    route.RoutingDecisionID,
			RoutingCandidateID:   candidate.RoutingCandidateID,
			RoutingFingerprint:   route.RoutingFingerprint,
			CandidateFingerprint: candidate.CandidateFingerprint,
			ActualIndependence:   actualIndependence(worker, candidate),
		})
	}
	if !input.CouncilLimits.Enabled && len(authorities) > 1 {
		disagreements = append(disagreements, "non-council verification cannot declare multiple member authorities")
		terminal = taskrequirements.ErrVerificationDecisionConflictCode
	}
	if input.CouncilLimits.Enabled && len(authorities) == 0 {
		disagreements = append(disagreements, "bounded council requires explicit verifier member authority")
		terminal = taskrequirements.ErrVerificationDecisionConflictCode
	}
	sort.Slice(authorities, func(i, j int) bool {
		if authorities[i].RoutingFingerprint != authorities[j].RoutingFingerprint {
			return authorities[i].RoutingFingerprint < authorities[j].RoutingFingerprint
		}
		return authorities[i].RoutingDecisionID < authorities[j].RoutingDecisionID
	})
	return authorities, disagreements, terminal, nil
}

func explicitCouncilMemberRouteIDs(input VerificationDecisionInput, verifierRoute RoutingDecision) []string {
	values := input.CouncilMemberRoutingDecisionIDs
	if len(values) == 0 && verifierRoute.RoutingDecisionID != "" {
		values = []string{verifierRoute.RoutingDecisionID}
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func validVerifierIdentity(value string) bool {
	if value == "" || len(value) > 120 {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' || r == ':' {
			continue
		}
		return false
	}
	return true
}

func allowedVerifierVerdictAuthority(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "verifier", "council-member":
		return true
	default:
		return false
	}
}

func aggregateCouncil(limits CouncilLimits, roundsUsed int, budgetUsed int64, timedOut bool, memberCount int, verdicts []VerifierVerdict, now time.Time) CouncilState {
	state := CouncilState{
		Enabled:          limits.Enabled,
		MaxMembers:       limits.MaxMembers,
		MaxRounds:        limits.MaxRounds,
		MaxDurationMS:    limits.MaxDurationMS,
		MaxBudgetTokens:  limits.MaxBudgetTokens,
		MemberCount:      memberCount,
		RoundsUsed:       roundsUsed,
		BudgetTokensUsed: budgetUsed,
		Outcome:          VerificationVerdictPass,
	}
	if !limits.Enabled {
		if len(verdicts) == 0 {
			state.Outcome = VerificationVerdictNeedsHuman
			return state
		}
		return aggregateVerdicts(state, verdicts)
	}
	switch {
	case limits.MaxMembers <= 0 || memberCount <= 0 || memberCount > limits.MaxMembers:
		state.BoundExceeded, state.TerminalErrorCode = "members", taskrequirements.ErrVerifierCouncilBoundExceededCode
	case limits.MaxRounds <= 0 || roundsUsed <= 0 || roundsUsed > limits.MaxRounds:
		state.BoundExceeded, state.TerminalErrorCode = "rounds", taskrequirements.ErrVerifierCouncilBoundExceededCode
	case limits.MaxBudgetTokens <= 0 || budgetUsed < 0 || budgetUsed > limits.MaxBudgetTokens:
		state.BoundExceeded, state.TerminalErrorCode = "budget", taskrequirements.ErrVerifierCouncilBoundExceededCode
	case limits.MaxDurationMS <= 0 || limits.MaxDurationMS > maxCouncilDurationMS:
		state.BoundExceeded, state.TerminalErrorCode = "time", taskrequirements.ErrVerifierCouncilBoundExceededCode
	}
	if deadline, ok := parseCouncilDeadline(limits, now); timedOut || !ok || !now.Before(deadline) {
		state.TimedOut = true
		state.BoundExceeded = "time"
		state.TerminalErrorCode = taskrequirements.ErrVerifierCouncilBoundExceededCode
	}
	if state.TerminalErrorCode != "" {
		state.Outcome = VerificationVerdictNeedsHuman
		return state
	}
	return aggregateVerdicts(state, verdicts)
}

func aggregateVerdicts(state CouncilState, verdicts []VerifierVerdict) CouncilState {
	if len(verdicts) == 0 {
		state.Outcome = VerificationVerdictNeedsHuman
		return state
	}
	sawFail, sawNeedsHuman, sawPass := false, false, false
	for _, verdict := range verdicts {
		switch verdict.Verdict {
		case VerificationVerdictPass:
			sawPass = true
		case VerificationVerdictFail:
			sawFail = true
		default:
			sawNeedsHuman = true
		}
	}
	switch {
	case sawNeedsHuman || (sawFail && sawPass):
		state.Outcome = VerificationVerdictNeedsHuman
	case sawFail:
		state.Outcome = VerificationVerdictFail
	default:
		state.Outcome = VerificationVerdictPass
	}
	return state
}

func insertVerificationDecisionTx(ctx context.Context, tx storage.Tx, decision VerificationDecision) error {
	payload, err := delivery.CanonicalJSON(decision)
	if err != nil {
		return err
	}
	disagreements, err := canonicalString(decision.Disagreements)
	if err != nil {
		return err
	}
	evidence, err := canonicalString(decision.EvidenceRefs)
	if err != nil {
		return err
	}
	council, err := canonicalString(decision.Council)
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
	result, err := tx.Exec(ctx, `INSERT INTO verification_decisions(
		verification_decision_id, schema_version, record_version, project_id, delivery_run_id, task_id,
		task_requirement_id, worker_routing_decision_id, verifier_routing_decision_id, decision_key, idempotency_key,
		decision_status, verdict, required_independence, actual_independence, final_authority,
		final_authority_fingerprint, terminal_error_code, verification_fingerprint, policy_fingerprint, plan_fingerprint,
		disagreements_json, evidence_refs_json, council_json, payload_json, created_at, updated_at, decided_by_json, host_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(verification_decision_id) DO NOTHING`,
		decision.VerificationDecisionID, decision.SchemaVersion, decision.RecordVersion, decision.ProjectID, decision.DeliveryRunID, decision.TaskID,
		decision.TaskRequirementID, decision.WorkerRoutingDecisionID, decision.VerifierRoutingDecisionID, decision.DecisionKey, decision.IdempotencyKey,
		decision.DecisionStatus, decision.Verdict, string(decision.RequiredIndependence), string(decision.ActualIndependence), decision.FinalAuthority,
		decision.FinalAuthorityFingerprint, string(decision.TerminalErrorCode), decision.VerificationFingerprint, decision.PolicyFingerprint, decision.PlanFingerprint,
		disagreements, evidence, council, string(payload), decision.CreatedAt, decision.UpdatedAt, actor, host)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 0 {
		if err != nil {
			return err
		}
		return insertVerificationDecisionMembersTx(ctx, tx, decision)
	}
	var existing string
	if err := tx.QueryRow(ctx, `SELECT payload_json FROM verification_decisions WHERE verification_decision_id = ?`, decision.VerificationDecisionID).Scan(&existing); err != nil {
		return err
	}
	if existing != string(payload) {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrDuplicateRecordCode, Message: "verification decision id already exists with different payload"}
	}
	return nil
}

func insertVerificationDecisionMembersTx(ctx context.Context, tx storage.Tx, decision VerificationDecision) error {
	for ordinal, member := range canonicalCouncilMemberAuthorities(decision.CouncilMemberAuthorities) {
		payload, err := canonicalString(member)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO verification_decision_members(
			verification_decision_id, member_ordinal, member_id, routing_decision_id, routing_candidate_id,
			routing_fingerprint, candidate_fingerprint, actual_independence, payload_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(verification_decision_id, member_ordinal) DO NOTHING`,
			decision.VerificationDecisionID, ordinal, member.MemberID, member.RoutingDecisionID, member.RoutingCandidateID,
			member.RoutingFingerprint, member.CandidateFingerprint, string(member.ActualIndependence), payload)
		if err != nil {
			return err
		}
	}
	return nil
}

func LoadVerificationDecision(ctx context.Context, store storage.Store, id string) (VerificationDecision, error) {
	var payload string
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT payload_json FROM verification_decisions WHERE verification_decision_id = ?`, strings.TrimSpace(id)).Scan(&payload)
	})
	if err != nil {
		return VerificationDecision{}, err
	}
	var decision VerificationDecision
	if err := json.Unmarshal([]byte(payload), &decision); err != nil {
		return VerificationDecision{}, err
	}
	return decision, nil
}

func ExplainVerificationJSON(decision VerificationDecision) ([]byte, error) {
	return delivery.CanonicalJSON(redactedVerificationDecision(decision))
}

func ExplainVerificationHuman(decision VerificationDecision) string {
	d := redactedVerificationDecision(decision)
	var b strings.Builder
	fmt.Fprintf(&b, "verification %s: verdict=%s final_authority=%s\n", d.DecisionStatus, d.Verdict, d.FinalAuthority)
	fmt.Fprintf(&b, "independence required=%s actual=%s worker=%s verifier=%s\n", d.RequiredIndependence, d.ActualIndependence, d.WorkerCandidateID, d.VerifierCandidateID)
	if d.Council.Enabled {
		fmt.Fprintf(&b, "council members=%d/%d rounds=%d/%d budget=%d/%d outcome=%s timeout=%t\n", d.Council.MemberCount, d.Council.MaxMembers, d.Council.RoundsUsed, d.Council.MaxRounds, d.Council.BudgetTokensUsed, d.Council.MaxBudgetTokens, d.Council.Outcome, d.Council.TimedOut)
	}
	for _, disagreement := range d.Disagreements {
		fmt.Fprintf(&b, "- disagreement: %s\n", disagreement)
	}
	for _, rejected := range d.EligibilityExclusions {
		fmt.Fprintf(&b, "- exclusion: %s %s\n", rejected.Code, sanitizeText(rejected.Message, 160))
	}
	return strings.TrimSpace(b.String())
}

func resolveVerifierRouteTx(ctx context.Context, tx storage.Tx, verifierRoutingDecisionID string) (RoutingDecision, Candidate, taskrequirements.TaskRequirement, []RejectionReason, error) {
	if strings.TrimSpace(verifierRoutingDecisionID) == "" {
		return RoutingDecision{}, Candidate{}, taskrequirements.TaskRequirement{}, nil, nil
	}
	route, err := loadRoutingDecisionTx(ctx, tx, verifierRoutingDecisionID)
	if err != nil {
		return RoutingDecision{}, Candidate{}, taskrequirements.TaskRequirement{}, nil, err
	}
	req, err := loadTaskRequirementTx(ctx, tx, route.TaskRequirementID)
	if err != nil {
		return RoutingDecision{}, Candidate{}, taskrequirements.TaskRequirement{}, nil, err
	}
	candidate, _ := chosenCandidate(route)
	var exclusions []RejectionReason
	for _, rejected := range route.RejectedCandidates {
		exclusions = append(exclusions, rejected.Reasons...)
	}
	return route, candidate, req, exclusions, nil
}

func chosenCandidate(decision RoutingDecision) (Candidate, bool) {
	for _, candidate := range decision.EligibleCandidates {
		if candidate.RoutingCandidateID == decision.ChosenCandidateID {
			return candidate, true
		}
	}
	for _, scored := range decision.ScoredCandidates {
		if scored.RoutingCandidateID == decision.ChosenCandidateID {
			return scored.Candidate, true
		}
	}
	return Candidate{}, false
}

func actualIndependence(worker, verifier Candidate) taskrequirements.IndependenceLevel {
	if verifier.RoutingCandidateID == "" {
		return taskrequirements.IndependenceNone
	}
	if worker.AdapterID != verifier.AdapterID {
		return taskrequirements.IndependenceDifferentProvider
	}
	if worker.AccountProfileID != verifier.AccountProfileID {
		return taskrequirements.IndependenceDifferentAccount
	}
	if worker.ModelCapabilityID != verifier.ModelCapabilityID {
		return taskrequirements.IndependenceDifferentModel
	}
	return taskrequirements.IndependenceNone
}

func strongerIndependence(a, b taskrequirements.IndependenceLevel) taskrequirements.IndependenceLevel {
	if independenceRank(b) > independenceRank(a) {
		return b
	}
	return a
}

func workerVerificationOutputAllowed(req taskrequirements.TaskRequirement, profile RoutingPolicyProfile) bool {
	requiredOutput := taskrequirements.OutputVerificationVerdict
	if risk, ok := profile.RiskPolicy[req.RiskTier]; ok && risk.VerificationOutput != "" {
		requiredOutput = risk.VerificationOutput
	}
	for _, verification := range req.VerificationRequirements {
		if riskApplies(req.RiskTier, verification.RequiredForRiskTiers) && verification.OutputContract != "" {
			requiredOutput = verification.OutputContract
			break
		}
	}
	return requiredOutput == taskrequirements.OutputVerificationVerdict || requiredOutput == taskrequirements.OutputJSONSchema
}

func verifierRouteAllowed(req taskrequirements.TaskRequirement, verifier Candidate) bool {
	if !verifierRoleCompatible(req.RoleKey) || !verifierRoleCompatible(verifier.RoleKey) {
		return false
	}
	if req.PermissionRequired != taskrequirements.PermissionReadOnly || verifier.Permission != taskrequirements.PermissionReadOnly {
		return false
	}
	return req.RequiredOutput == taskrequirements.OutputVerificationVerdict || req.RequiredOutput == taskrequirements.OutputJSONSchema
}

func verifierRoleCompatible(roleKey string) bool {
	switch normalizeRoleKey(roleKey) {
	case RoleKeyVerifier, RoleKeySoul:
		return true
	default:
		return false
	}
}

func evidenceRefs(verdicts []VerifierVerdict) []EvidenceRef {
	var refs []EvidenceRef
	for _, verdict := range verdicts {
		refs = append(refs, verdict.EvidenceRefs...)
	}
	for i := range refs {
		refs[i].RecordKind = sanitizeIdentifier(refs[i].RecordKind)
		refs[i].RecordID = sanitizeIdentifier(refs[i].RecordID)
		refs[i].Summary = sanitizeText(refs[i].Summary, 160)
	}
	refs = dedupeEvidenceRefs(refs)
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].RecordKind != refs[j].RecordKind {
			return refs[i].RecordKind < refs[j].RecordKind
		}
		return refs[i].RecordID < refs[j].RecordID
	})
	if len(refs) > 32 {
		refs = refs[:32]
	}
	return refs
}

func councilDisagreements(verdicts []VerifierVerdict, council CouncilState) []string {
	var out []string
	if council.BoundExceeded != "" {
		out = append(out, "council "+council.BoundExceeded+" bound expired or was exceeded")
	}
	first := ""
	for _, verdict := range verdicts {
		if first == "" {
			first = verdict.Verdict
			continue
		}
		if verdict.Verdict != first {
			out = append(out, "verifier verdicts disagree")
			break
		}
	}
	for _, verdict := range verdicts {
		if verdict.Verdict == VerificationVerdictNeedsHuman || verdict.TerminalErrorCode != "" {
			out = append(out, sanitizeText(firstNonEmpty(verdict.Message, string(verdict.TerminalErrorCode)), 160))
		}
	}
	return out
}

func sanitizeVerifierVerdicts(verdicts []VerifierVerdict) []VerifierVerdict {
	out := make([]VerifierVerdict, 0, len(verdicts))
	for _, verdict := range verdicts {
		verdict.MemberID = sanitizeIdentifier(verdict.MemberID)
		verdict.RoutingCandidateID = sanitizeIdentifier(verdict.RoutingCandidateID)
		verdict.Message = sanitizeText(verdict.Message, 256)
		verdict.Authority = sanitizeIdentifier(verdict.Authority)
		verdict.AuthorityFingerprint = sanitizeIdentifier(verdict.AuthorityFingerprint)
		verdict.VerificationStartedAt = sanitizeIdentifier(verdict.VerificationStartedAt)
		verdict.VerificationEndedAt = sanitizeIdentifier(verdict.VerificationEndedAt)
		verdict.EvidenceRefs = evidenceRefs([]VerifierVerdict{verdict})
		out = append(out, verdict)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MemberID != out[j].MemberID {
			return out[i].MemberID < out[j].MemberID
		}
		return out[i].RoutingCandidateID < out[j].RoutingCandidateID
	})
	return out
}

func redactedVerificationDecision(decision VerificationDecision) VerificationDecision {
	decision.VerificationDecisionID = sanitizeIdentifier(decision.VerificationDecisionID)
	decision.ProjectID = sanitizeIdentifier(decision.ProjectID)
	decision.DeliveryRunID = sanitizeIdentifier(decision.DeliveryRunID)
	decision.TaskID = sanitizeIdentifier(decision.TaskID)
	decision.TaskRequirementID = sanitizeIdentifier(decision.TaskRequirementID)
	decision.DecisionKey = sanitizeIdentifier(decision.DecisionKey)
	decision.IdempotencyKey = sanitizeIdentifier(decision.IdempotencyKey)
	decision.DecidedBy.ActorID = sanitizeIdentifier(decision.DecidedBy.ActorID)
	decision.DecidedBy.Display = sanitizeText(decision.DecidedBy.Display, 80)
	decision.Host.HostID = sanitizeIdentifier(decision.Host.HostID)
	decision.Host.SessionID = sanitizeIdentifier(decision.Host.SessionID)
	decision.WorkerCandidateID = sanitizeIdentifier(decision.WorkerCandidateID)
	decision.VerifierCandidateID = sanitizeIdentifier(decision.VerifierCandidateID)
	decision.WorkerRoutingDecisionID = sanitizeIdentifier(decision.WorkerRoutingDecisionID)
	decision.VerifierRoutingDecisionID = sanitizeIdentifier(decision.VerifierRoutingDecisionID)
	decision.VerifierVerdicts = sanitizeVerifierVerdicts(decision.VerifierVerdicts)
	for i := range decision.CouncilMemberAuthorities {
		decision.CouncilMemberAuthorities[i].MemberID = sanitizeIdentifier(decision.CouncilMemberAuthorities[i].MemberID)
		decision.CouncilMemberAuthorities[i].RoutingDecisionID = sanitizeIdentifier(decision.CouncilMemberAuthorities[i].RoutingDecisionID)
		decision.CouncilMemberAuthorities[i].RoutingCandidateID = sanitizeIdentifier(decision.CouncilMemberAuthorities[i].RoutingCandidateID)
		decision.CouncilMemberAuthorities[i].RoutingFingerprint = sanitizeIdentifier(decision.CouncilMemberAuthorities[i].RoutingFingerprint)
		decision.CouncilMemberAuthorities[i].CandidateFingerprint = sanitizeIdentifier(decision.CouncilMemberAuthorities[i].CandidateFingerprint)
	}
	for i := range decision.Disagreements {
		decision.Disagreements[i] = sanitizeText(decision.Disagreements[i], 160)
	}
	for i := range decision.EligibilityExclusions {
		decision.EligibilityExclusions[i].Message = sanitizeText(decision.EligibilityExclusions[i].Message, 160)
		decision.EligibilityExclusions[i].EvidenceRecordIDs = boundedSanitizedIDs(decision.EligibilityExclusions[i].EvidenceRecordIDs, 16)
		decision.EligibilityExclusions[i].SnapshotIDs = boundedSanitizedIDs(decision.EligibilityExclusions[i].SnapshotIDs, 16)
	}
	decision.EvidenceRefs = evidenceRefs([]VerifierVerdict{{EvidenceRefs: decision.EvidenceRefs}})
	return decision
}

var secretLikePatterns = []*regexp.Regexp{
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`(?i)(github_pat|gh[pousr]_|sk-[a-z0-9_-]{12,}|xox[baprs]-)`),
	regexp.MustCompile(`(?i)(token|secret|password|api[_-]?key)=\S+`),
	regexp.MustCompile(`(?i)bearer\s+[a-z0-9._~+/=-]{12,}`),
	regexp.MustCompile(`[A-Za-z]:\\[^\s"']{8,}`),
	regexp.MustCompile(`/[A-Za-z0-9._@%+=:,/-]{12,}`),
}

func validateAuthorityKey(label, value string, required bool) error {
	if value == "" && !required {
		return nil
	}
	if value == "" {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: label + " is required"}
	}
	if !utf8.ValidString(value) || strings.TrimSpace(value) != value || len(value) > maxAuthorityKeyBytes {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: label + " has invalid authority-key syntax"}
	}
	for _, pattern := range secretLikePatterns {
		if pattern.MatchString(value) {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: label + " has invalid authority-key syntax"}
		}
	}
	for i, r := range value {
		if i == 0 && !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: label + " has invalid authority-key syntax"}
		}
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' || r == ':' {
			continue
		}
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: label + " has invalid authority-key syntax"}
	}
	return nil
}

func sanitizeText(value string, max int) string {
	value = strings.ToValidUTF8(value, "")
	value = strings.TrimSpace(value)
	for _, pattern := range secretLikePatterns {
		value = pattern.ReplaceAllString(value, "[REDACTED]")
	}
	if max > 0 {
		runes := []rune(value)
		if len(runes) > max {
			return string(runes[:max])
		}
	}
	return value
}

func sanitizeIdentifier(value string) string {
	return sanitizeText(value, 80)
}

func boundedSanitizedIDs(values []string, max int) []string {
	values = dedupeStrings(values)
	if len(values) > max {
		values = values[:max]
	}
	for i := range values {
		values[i] = sanitizeIdentifier(values[i])
	}
	return values
}

func dedupeEvidenceRefs(values []EvidenceRef) []EvidenceRef {
	out := make([]EvidenceRef, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		key := value.RecordKind + "\x00" + value.RecordID + "\x00" + value.Summary
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func verificationRequestFingerprint(input VerificationDecisionInput, workerRoute, verifierRoute RoutingDecision, memberAuthorities []CouncilMemberAuthority) string {
	digest, _, err := delivery.DigestCanonicalJSON(map[string]any{
		"schema_version":               "loopcoder.verification_request.v1",
		"worker_routing_decision_id":   workerRoute.RoutingDecisionID,
		"worker_routing_fingerprint":   workerRoute.RoutingFingerprint,
		"verifier_routing_decision_id": verifierRoute.RoutingDecisionID,
		"verifier_routing_fingerprint": verifierRoute.RoutingFingerprint,
		"decision_key":                 input.DecisionKey,
		"authority_fingerprint":        strings.TrimSpace(input.AuthorityFingerprint),
		"verifier_verdicts":            canonicalVerifierVerdicts(input.VerifierVerdicts),
		"council_member_authorities":   canonicalCouncilMemberAuthorities(memberAuthorities),
		"council_limits":               input.CouncilLimits,
		"council_rounds_used":          input.CouncilRoundsUsed,
		"council_budget_tokens_used":   input.CouncilBudgetTokensUsed,
		"timeout":                      input.Timeout,
	})
	if err != nil {
		return ""
	}
	return digest
}

func verificationIdempotencyKey(input VerificationDecisionInput, workerRoute, verifierRoute RoutingDecision, memberAuthorities []CouncilMemberAuthority) string {
	key := input.IdempotencyKey
	if key != "" {
		return key
	}
	return "verification-event:" + verificationRequestFingerprint(input, workerRoute, verifierRoute, memberAuthorities)
}

func canonicalVerifierVerdicts(verdicts []VerifierVerdict) []VerifierVerdict {
	out := make([]VerifierVerdict, 0, len(verdicts))
	for _, verdict := range verdicts {
		verdict.MemberID = strings.ToValidUTF8(strings.TrimSpace(verdict.MemberID), "")
		verdict.RoutingCandidateID = strings.ToValidUTF8(strings.TrimSpace(verdict.RoutingCandidateID), "")
		verdict.Verdict = strings.ToValidUTF8(strings.TrimSpace(verdict.Verdict), "")
		verdict.Message = strings.ToValidUTF8(strings.TrimSpace(verdict.Message), "")
		verdict.Authority = strings.ToValidUTF8(strings.TrimSpace(verdict.Authority), "")
		verdict.AuthorityFingerprint = strings.ToValidUTF8(strings.TrimSpace(verdict.AuthorityFingerprint), "")
		verdict.VerificationStartedAt = strings.ToValidUTF8(strings.TrimSpace(verdict.VerificationStartedAt), "")
		verdict.VerificationEndedAt = strings.ToValidUTF8(strings.TrimSpace(verdict.VerificationEndedAt), "")
		for i := range verdict.EvidenceRefs {
			verdict.EvidenceRefs[i].RecordKind = strings.ToValidUTF8(strings.TrimSpace(verdict.EvidenceRefs[i].RecordKind), "")
			verdict.EvidenceRefs[i].RecordID = strings.ToValidUTF8(strings.TrimSpace(verdict.EvidenceRefs[i].RecordID), "")
			verdict.EvidenceRefs[i].Summary = strings.ToValidUTF8(strings.TrimSpace(verdict.EvidenceRefs[i].Summary), "")
		}
		sort.Slice(verdict.EvidenceRefs, func(i, j int) bool {
			if verdict.EvidenceRefs[i].RecordKind != verdict.EvidenceRefs[j].RecordKind {
				return verdict.EvidenceRefs[i].RecordKind < verdict.EvidenceRefs[j].RecordKind
			}
			if verdict.EvidenceRefs[i].RecordID != verdict.EvidenceRefs[j].RecordID {
				return verdict.EvidenceRefs[i].RecordID < verdict.EvidenceRefs[j].RecordID
			}
			return verdict.EvidenceRefs[i].Summary < verdict.EvidenceRefs[j].Summary
		})
		out = append(out, verdict)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AuthorityFingerprint != out[j].AuthorityFingerprint {
			return out[i].AuthorityFingerprint < out[j].AuthorityFingerprint
		}
		if out[i].RoutingCandidateID != out[j].RoutingCandidateID {
			return out[i].RoutingCandidateID < out[j].RoutingCandidateID
		}
		return out[i].MemberID < out[j].MemberID
	})
	return out
}

func canonicalCouncilMemberAuthorities(authorities []CouncilMemberAuthority) []CouncilMemberAuthority {
	out := append([]CouncilMemberAuthority(nil), authorities...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].RoutingFingerprint != out[j].RoutingFingerprint {
			return out[i].RoutingFingerprint < out[j].RoutingFingerprint
		}
		return out[i].RoutingDecisionID < out[j].RoutingDecisionID
	})
	return out
}

func verificationDecisionID(projectID, deliveryRunID, taskID, decisionKey, authorityFingerprint, verificationFingerprint string) string {
	return "vdec_" + digestBase32(projectID, deliveryRunID, taskID, decisionKey, authorityFingerprint, verificationFingerprint)
}

func loadVerificationByIdempotency(ctx context.Context, tx storage.Tx, deliveryRunID, taskID, decisionKey, key string) (VerificationDecision, bool, error) {
	var payload string
	err := tx.QueryRow(ctx, `SELECT payload_json FROM verification_decisions
		WHERE delivery_run_id = ? AND task_id = ? AND decision_key = ? AND idempotency_key = ?`,
		strings.TrimSpace(deliveryRunID), strings.TrimSpace(taskID), strings.TrimSpace(decisionKey), strings.TrimSpace(key)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return VerificationDecision{}, false, nil
	}
	if err != nil {
		return VerificationDecision{}, false, err
	}
	var decision VerificationDecision
	if err := json.Unmarshal([]byte(payload), &decision); err != nil {
		return VerificationDecision{}, false, err
	}
	return decision, true, nil
}

func loadVerificationByAuthority(ctx context.Context, tx storage.Tx, deliveryRunID, taskID, decisionKey, authorityFingerprint string) (VerificationDecision, bool, error) {
	var payload string
	err := tx.QueryRow(ctx, `SELECT payload_json FROM verification_decisions
		WHERE delivery_run_id = ? AND task_id = ? AND decision_key = ? AND final_authority_fingerprint = ?
		ORDER BY created_at, verification_decision_id LIMIT 1`,
		strings.TrimSpace(deliveryRunID), strings.TrimSpace(taskID), strings.TrimSpace(decisionKey), strings.TrimSpace(authorityFingerprint)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return VerificationDecision{}, false, nil
	}
	if err != nil {
		return VerificationDecision{}, false, err
	}
	var decision VerificationDecision
	if err := json.Unmarshal([]byte(payload), &decision); err != nil {
		return VerificationDecision{}, false, err
	}
	return decision, true, nil
}

func validateVerificationDecision(decision VerificationDecision) error {
	if decision.SchemaVersion != VerificationDecisionSchema || decision.RecordVersion != 1 ||
		decision.VerificationDecisionID == "" || decision.ProjectID == "" || decision.DeliveryRunID == "" ||
		decision.TaskID == "" || decision.TaskRequirementID == "" || decision.WorkerRoutingDecisionID == "" ||
		decision.DecisionKey == "" || decision.IdempotencyKey == "" || decision.DecisionStatus == "" ||
		decision.Verdict == "" || decision.RequiredIndependence == "" || decision.ActualIndependence == "" ||
		decision.FinalAuthority == "" || decision.FinalAuthorityFingerprint == "" || decision.VerificationFingerprint == "" ||
		decision.PolicyFingerprint == "" || decision.PlanFingerprint == "" || decision.CreatedAt == "" || decision.UpdatedAt == "" {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "verification decision has missing required fields"}
	}
	if !validFingerprint(decision.FinalAuthorityFingerprint) || !validFingerprint(decision.VerificationFingerprint) || !validFingerprint(decision.PolicyFingerprint) || !validFingerprint(decision.PlanFingerprint) {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "verification decision fingerprints must be sha256 digests"}
	}
	if decision.Council.Enabled && len(decision.CouncilMemberAuthorities) != decision.Council.MemberCount {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "council member authority count must match council member count"}
	}
	for _, authority := range decision.CouncilMemberAuthorities {
		if authority.RoutingDecisionID == "" || authority.RoutingCandidateID == "" || authority.RoutingFingerprint == "" || authority.CandidateFingerprint == "" || authority.ActualIndependence == "" {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "council member authority has missing required fields"}
		}
		if !validFingerprint(authority.RoutingFingerprint) || !validFingerprint(authority.CandidateFingerprint) {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "council member authority fingerprints must be sha256 digests"}
		}
	}
	switch decision.DecisionStatus {
	case VerificationStatusAccepted:
		if decision.Verdict != VerificationVerdictPass || decision.TerminalErrorCode != "" || decision.FinalAuthority == FinalAuthorityHuman {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "accepted verification decision must be automated pass without terminal error"}
		}
	case VerificationStatusRejected:
		if decision.Verdict != VerificationVerdictFail || decision.TerminalErrorCode == "" {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "rejected verification decision must carry fail verdict and terminal error"}
		}
	case VerificationStatusNeedsHuman:
		if decision.Verdict != VerificationVerdictNeedsHuman || decision.FinalAuthority != FinalAuthorityHuman || decision.TerminalErrorCode == "" {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "needs-human verification decision must carry human authority and terminal error"}
		}
	default:
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "unknown verification decision status"}
	}
	return nil
}

func verificationTerminalError(code taskrequirements.ErrorCode) error {
	switch code {
	case taskrequirements.ErrNoEligibleCandidateCode:
		return taskrequirements.ErrNoEligibleCandidate
	case taskrequirements.ErrVerifierIndependenceRequiredCode:
		return &taskrequirements.TypedError{Code: taskrequirements.ErrVerifierIndependenceRequiredCode}
	case taskrequirements.ErrVerifierCouncilBoundExceededCode:
		return taskrequirements.ErrVerifierCouncilBoundExceeded
	case taskrequirements.ErrVerificationDecisionConflictCode:
		return taskrequirements.ErrVerificationDecisionConflict
	case taskrequirements.ErrRequirementConfidenceInsufficientCode:
		return taskrequirements.ErrRequirementConfidenceInsufficient
	case taskrequirements.ErrCapabilityUnsupportedCode:
		return taskrequirements.ErrCapabilityUnsupported
	default:
		return &taskrequirements.TypedError{Code: code}
	}
}

func parseCouncilDeadline(limits CouncilLimits, now time.Time) (time.Time, bool) {
	if strings.TrimSpace(limits.StartedAt) == "" || strings.TrimSpace(limits.DeadlineAt) == "" || limits.MaxDurationMS <= 0 || limits.MaxDurationMS > maxCouncilDurationMS {
		return time.Time{}, false
	}
	started, err := time.Parse(time.RFC3339Nano, limits.StartedAt)
	if err != nil || limits.StartedAt != started.Format(time.RFC3339Nano) || started.After(now) {
		return time.Time{}, false
	}
	deadline, err := time.Parse(time.RFC3339Nano, limits.DeadlineAt)
	if err != nil || limits.DeadlineAt != deadline.Format(time.RFC3339Nano) {
		return time.Time{}, false
	}
	capDeadline := started.Add(time.Duration(limits.MaxDurationMS) * time.Millisecond)
	if deadline.After(capDeadline) {
		return time.Time{}, false
	}
	return deadline, true
}

func nonNilRejectionReasons(values []RejectionReason) []RejectionReason {
	if values == nil {
		return []RejectionReason{}
	}
	return values
}
