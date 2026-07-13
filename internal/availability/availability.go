// Package availability derives deterministic provider availability read models.
package availability

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/budget"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/usageledger"
)

const (
	ObservationSchema = "loopcoder.availability_observation.v1"
	ScoreSchema       = "loopcoder.availability_score.v1"
	BreakerSchema     = "loopcoder.circuit_breaker.v1"
	PolicyVersion     = "availability-read-model-v1"
)

var (
	ErrAvailabilityUnknown = errors.New("ErrAvailabilityUnknown")
	ErrRateLimited         = errors.New("ErrRateLimited")
	ErrBreakerOpen         = errors.New("ErrBreakerOpen")
	ErrAvailabilityRecord  = errors.New("ErrAvailabilityRecordMalformed")
)

type ObservationKind string

const (
	ObservationProbeSuccess      ObservationKind = "probe-success"
	ObservationProbeFailure      ObservationKind = "probe-failure"
	ObservationAuthFailure       ObservationKind = "auth-failure"
	ObservationQuotaExhausted    ObservationKind = "quota-exhausted"
	ObservationRateLimited       ObservationKind = "rate-limited"
	ObservationModelUnavailable  ObservationKind = "model-unavailable"
	ObservationTransportFailure  ObservationKind = "transport-failure"
	ObservationProviderOutage    ObservationKind = "provider-outage"
	ObservationMalformedResponse ObservationKind = "malformed-response"
	ObservationLaunchSuccess     ObservationKind = "launch-success"
	ObservationLaunchFailure     ObservationKind = "launch-failure"
)

type ReasonCode string

const (
	ReasonQuota                       ReasonCode = "quota"
	ReasonQuotaExhausted              ReasonCode = "quota-exhausted"
	ReasonQuotaConfidenceInsufficient ReasonCode = "quota-confidence-insufficient"
	ReasonRateLimited429              ReasonCode = "rate-limited-429"
	ReasonAuth                        ReasonCode = "auth"
	ReasonModelUnavailable            ReasonCode = "model-unavailable"
	ReasonTransport                   ReasonCode = "transport"
	ReasonProviderOutage              ReasonCode = "provider-outage"
	ReasonMalformedResponse           ReasonCode = "malformed-response"
	ReasonStaleEvidence               ReasonCode = "stale-evidence"
	ReasonUnknownTelemetry            ReasonCode = "unknown-telemetry"
	ReasonBudgetExhausted             ReasonCode = "budget-exhausted"
	ReasonOpenBreaker                 ReasonCode = "open-breaker"
	ReasonMissingHardCapability       ReasonCode = "missing-hard-capability"
	ReasonInstallationUnavailable     ReasonCode = "installation-unavailable"
)

type BreakerKind string

const (
	BreakerQuota     BreakerKind = "quota"
	BreakerRateLimit BreakerKind = "rate-limit"
	BreakerAuth      BreakerKind = "auth"
	BreakerHealth    BreakerKind = "health"
	BreakerModel     BreakerKind = "model"
	BreakerTransport BreakerKind = "transport"
)

type BreakerState string

const (
	BreakerClosed   BreakerState = "closed"
	BreakerOpen     BreakerState = "open"
	BreakerHalfOpen BreakerState = "half-open"
)

type Scope struct {
	ProjectID              string `json:"project_id,omitempty"`
	DeliveryRunID          string `json:"delivery_run_id,omitempty"`
	TaskID                 string `json:"task_id,omitempty"`
	AdapterID              string `json:"adapter_id,omitempty"`
	ProviderInstallationID string `json:"provider_installation_id,omitempty"`
	AccountProfileID       string `json:"account_profile_id,omitempty"`
	ModelCapabilityID      string `json:"model_capability_id,omitempty"`
	CanonicalModelID       string `json:"canonical_model_id,omitempty"`
}

type Observation struct {
	SchemaVersion             string                              `json:"schema_version"`
	RecordVersion             int                                 `json:"record_version"`
	AvailabilityObservationID string                              `json:"availability_observation_id"`
	ObservationKind           ObservationKind                     `json:"observation_kind"`
	ScopeKey                  string                              `json:"scope_key"`
	Scope                     Scope                               `json:"scope"`
	SourceRecordIDs           []string                            `json:"source_record_ids"`
	ObservedAt                string                              `json:"observed_at"`
	FailureClass              ReasonCode                          `json:"failure_class,omitempty"`
	RetryAfter                string                              `json:"retry_after,omitempty"`
	CooldownUntil             string                              `json:"cooldown_until,omitempty"`
	Confidence                providerinventory.Confidence        `json:"confidence"`
	NetworkDeclared           *bool                               `json:"network_declared,omitempty"`
	NetworkPermission         providerinventory.NetworkPermission `json:"network_permission,omitempty"`
	ReasonCodes               []ReasonCode                        `json:"reason_codes"`
	GapReasons                []string                            `json:"gap_reasons"`
}

type Component struct {
	Name              string                           `json:"name"`
	Score             int                              `json:"score"`
	MaxScore          int                              `json:"max_score"`
	Confidence        providerinventory.Confidence     `json:"confidence"`
	FreshnessState    providerinventory.FreshnessState `json:"freshness_state"`
	Hard              bool                             `json:"hard"`
	ReasonCodes       []ReasonCode                     `json:"reason_codes"`
	EvidenceRecordIDs []string                         `json:"evidence_record_ids"`
	Explanation       string                           `json:"explanation"`
}

type Score struct {
	SchemaVersion         string                       `json:"schema_version"`
	RecordVersion         int                          `json:"record_version"`
	AvailabilityScoreID   string                       `json:"availability_score_id"`
	ScopeKey              string                       `json:"scope_key"`
	Scope                 Scope                        `json:"scope"`
	Score                 int                          `json:"score"`
	Eligible              bool                         `json:"eligible"`
	ScoreConfidence       providerinventory.Confidence `json:"score_confidence"`
	Components            []Component                  `json:"components"`
	HardIneligibleReasons []ReasonCode                 `json:"hard_ineligible_reasons"`
	EvidenceRecordIDs     []string                     `json:"evidence_record_ids"`
	ObservationIDs        []string                     `json:"availability_observation_ids"`
	CircuitBreakerIDs     []string                     `json:"circuit_breaker_ids"`
	QuotaSnapshotIDs      []string                     `json:"quota_snapshot_ids"`
	UsageRecordIDs        []string                     `json:"usage_record_ids"`
	BudgetPolicyIDs       []string                     `json:"budget_policy_ids"`
	BudgetReservationIDs  []string                     `json:"budget_reservation_ids"`
	Heuristic             bool                         `json:"heuristic"`
	PolicyVersion         string                       `json:"policy_version"`
	CapturedAt            string                       `json:"captured_at"`
	GapReasons            []string                     `json:"gap_reasons"`
}

type CircuitBreaker struct {
	SchemaVersion       string       `json:"schema_version"`
	CircuitBreakerID    string       `json:"circuit_breaker_id"`
	BreakerKind         BreakerKind  `json:"breaker_kind"`
	State               BreakerState `json:"state"`
	ScopeKey            string       `json:"scope_key"`
	Scope               Scope        `json:"scope"`
	OpenedAt            string       `json:"opened_at,omitempty"`
	OpenUntil           string       `json:"open_until,omitempty"`
	HalfOpenProbeBudget int          `json:"half_open_probe_budget"`
	HalfOpenProbeCount  int          `json:"half_open_probe_count"`
	FailureCount        int          `json:"failure_count"`
	SuccessCount        int          `json:"success_count"`
	LastObservationID   string       `json:"last_observation_id,omitempty"`
	StateReason         ReasonCode   `json:"state_reason"`
	PolicyVersion       string       `json:"policy_version"`
	RecordVersion       int          `json:"record_version"`
}

type Policy struct {
	ExactQuotaRequired          bool
	ConservativeLocalScheduling bool
	RecentFailureWindow         time.Duration
	FailureThreshold            int
	HardRequirement             providerinventory.HardRequirement
}

type Inputs struct {
	Inventory       providerinventory.Report
	UsageSummaries  []usageledger.UsageSummary
	BudgetSummaries []budget.Summary
	Observations    []Observation
	CircuitBreakers []CircuitBreaker
	Policy          Policy
	Now             time.Time
}

type Result struct {
	Observations    []Observation    `json:"availability_observations"`
	Scores          []Score          `json:"availability_scores"`
	CircuitBreakers []CircuitBreaker `json:"circuit_breakers"`
}

func Derive(inputs Inputs) Result {
	now := inputs.Now.UTC()
	if now.IsZero() {
		now = time.Unix(0, 0).UTC()
	}
	policy := normalizePolicy(inputs.Policy)
	observations := append([]Observation(nil), inputs.Observations...)
	observations = append(observations, deriveInventoryObservations(inputs.Inventory, now)...)
	observations = dedupeObservations(observations)
	sortObservations(observations)

	scopes := scoreScopes(inputs.Inventory, inputs.BudgetSummaries)
	scores := make([]Score, 0, len(scopes))
	for _, scope := range scopes {
		score := scoreScope(scope, inputs, observations, policy, now)
		scores = append(scores, score)
	}
	sort.Slice(scores, func(i, j int) bool {
		if scores[i].Scope.AdapterID != scores[j].Scope.AdapterID {
			return scores[i].Scope.AdapterID < scores[j].Scope.AdapterID
		}
		return scores[i].ScopeKey < scores[j].ScopeKey
	})
	breakers := append([]CircuitBreaker(nil), inputs.CircuitBreakers...)
	sort.Slice(breakers, func(i, j int) bool { return breakers[i].CircuitBreakerID < breakers[j].CircuitBreakerID })
	return Result{Observations: observations, Scores: scores, CircuitBreakers: breakers}
}

func Persist(ctx context.Context, store storage.Store, result Result) error {
	if store == nil {
		return errors.New("availability persist: storage store is required")
	}
	return store.WithWriteTx(ctx, func(tx storage.Tx) error {
		for _, observation := range result.Observations {
			if err := insertObservation(ctx, tx, observation); err != nil {
				return err
			}
		}
		for _, score := range result.Scores {
			if err := insertScore(ctx, tx, score); err != nil {
				return err
			}
		}
		return nil
	})
}

func Load(ctx context.Context, store storage.Store) (Result, error) {
	if store == nil {
		return Result{}, errors.New("availability load: storage store is required")
	}
	var result Result
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, `SELECT payload_json FROM availability_observations ORDER BY observed_at, availability_observation_id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				rows.Close()
				return err
			}
			var observation Observation
			if err := json.Unmarshal([]byte(payload), &observation); err != nil {
				rows.Close()
				return err
			}
			result.Observations = append(result.Observations, observation)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT payload_json FROM availability_scores ORDER BY captured_at, availability_score_id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				rows.Close()
				return err
			}
			var score Score
			if err := json.Unmarshal([]byte(payload), &score); err != nil {
				rows.Close()
				return err
			}
			result.Scores = append(result.Scores, score)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT payload_json FROM circuit_breakers ORDER BY circuit_breaker_id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				rows.Close()
				return err
			}
			var breaker CircuitBreaker
			if err := json.Unmarshal([]byte(payload), &breaker); err != nil {
				rows.Close()
				return err
			}
			result.CircuitBreakers = append(result.CircuitBreakers, breaker)
		}
		return rows.Close()
	})
	return result, err
}

func RenderScore(w io.Writer, score Score) error {
	score = normalizeScore(score)
	if _, err := fmt.Fprintf(w, "availability %s score=%d eligible=%t confidence=%s\n", score.ScopeKey, score.Score, score.Eligible, score.ScoreConfidence); err != nil {
		return err
	}
	if len(score.HardIneligibleReasons) > 0 {
		if _, err := fmt.Fprintf(w, "hard: %s\n", joinReasons(score.HardIneligibleReasons)); err != nil {
			return err
		}
	}
	for _, component := range score.Components {
		if _, err := fmt.Fprintf(w, "- %s: %d/%d confidence=%s reasons=%s evidence=%s\n", component.Name, component.Score, component.MaxScore, component.Confidence, joinReasons(component.ReasonCodes), strings.Join(component.EvidenceRecordIDs, ",")); err != nil {
			return err
		}
	}
	return nil
}

func NormalizeReasonCode(value string) ReasonCode {
	v := strings.ToLower(strings.TrimSpace(value))
	v = strings.ReplaceAll(v, "_", "-")
	switch {
	case v == "429" || strings.Contains(v, "429") || strings.Contains(v, "rate-limit") || strings.Contains(v, "rate limited") || strings.Contains(v, "too many requests"):
		return ReasonRateLimited429
	case strings.Contains(v, "auth") || strings.Contains(v, "unauthorized") || strings.Contains(v, "unauthenticated"):
		return ReasonAuth
	case strings.Contains(v, "model"):
		return ReasonModelUnavailable
	case strings.Contains(v, "transport") || strings.Contains(v, "timeout") || strings.Contains(v, "network"):
		return ReasonTransport
	case strings.Contains(v, "outage") || strings.Contains(v, "5xx"):
		return ReasonProviderOutage
	case strings.Contains(v, "malformed") || strings.Contains(v, "parse"):
		return ReasonMalformedResponse
	case strings.Contains(v, "stale") || strings.Contains(v, "expired"):
		return ReasonStaleEvidence
	case strings.Contains(v, "quota-confidence"):
		return ReasonQuotaConfidenceInsufficient
	case strings.Contains(v, "quota-exhaust") || (strings.Contains(v, "quota") && strings.Contains(v, "exhaust")):
		return ReasonQuotaExhausted
	case strings.Contains(v, "quota"):
		return ReasonQuota
	case strings.Contains(v, "budget"):
		return ReasonBudgetExhausted
	case strings.Contains(v, "breaker"):
		return ReasonOpenBreaker
	default:
		return ReasonUnknownTelemetry
	}
}

func deriveInventoryObservations(inventory providerinventory.Report, now time.Time) []Observation {
	var out []Observation
	for _, probe := range inventory.ProbeResults {
		scope := Scope{
			ProjectID:              ptrValue(probe.ProjectID),
			AdapterID:              probe.AdapterID,
			ProviderInstallationID: ptrValue(probe.ProviderInstallationID),
		}
		networkDeclared := probe.NetworkDeclared
		kind := ObservationProbeSuccess
		confidence := probe.Confidence
		var failure ReasonCode
		var reasons []ReasonCode
		gaps := append([]string(nil), probe.GapReasons...)
		if probe.Outcome != providerinventory.OutcomeInstalled {
			kind = ObservationProbeFailure
			failure = ReasonInstallationUnavailable
			reasons = append(reasons, ReasonInstallationUnavailable)
			if probe.TimedOut || probe.Killed || probe.Outcome == providerinventory.OutcomeProbeFailed {
				kind = ObservationTransportFailure
				failure = ReasonTransport
				reasons = append(reasons, ReasonTransport)
			}
			if probe.FreshnessState == providerinventory.FreshnessStale || probe.FreshnessState == providerinventory.FreshnessExpired || probe.Confidence == providerinventory.ConfidenceStale {
				reasons = append(reasons, ReasonStaleEvidence)
			}
		}
		out = append(out, normalizeObservation(Observation{
			ObservationKind:   kind,
			Scope:             scope,
			SourceRecordIDs:   []string{probe.ProbeResultID},
			ObservedAt:        firstNonEmpty(probe.CapturedAt, formatTime(now)),
			FailureClass:      failure,
			Confidence:        confidence,
			NetworkDeclared:   &networkDeclared,
			NetworkPermission: probe.NetworkPermission,
			ReasonCodes:       reasons,
			GapReasons:        gaps,
		}))
	}
	for _, readiness := range inventory.AuthReadiness {
		if readiness.ReadinessState == providerinventory.ReadinessReady && readiness.FreshnessState == providerinventory.FreshnessFresh && readiness.Confidence != providerinventory.ConfidenceStale {
			continue
		}
		scope := Scope{
			ProjectID:              ptrValue(readiness.ProjectID),
			AdapterID:              readiness.AdapterID,
			ProviderInstallationID: ptrValue(readiness.ProviderInstallationID),
			AccountProfileID:       ptrValue(readiness.AccountProfileID),
		}
		reasons := []ReasonCode{ReasonAuth}
		if readiness.FreshnessState == providerinventory.FreshnessStale || readiness.FreshnessState == providerinventory.FreshnessExpired || readiness.Confidence == providerinventory.ConfidenceStale {
			reasons = append(reasons, ReasonStaleEvidence)
		}
		out = append(out, normalizeObservation(Observation{
			ObservationKind: ObservationAuthFailure,
			Scope:           scope,
			SourceRecordIDs: []string{readiness.AuthReadinessID},
			ObservedAt:      firstNonEmpty(readiness.CapturedAt, formatTime(now)),
			FailureClass:    ReasonAuth,
			Confidence:      readiness.Confidence,
			ReasonCodes:     reasons,
			GapReasons:      readiness.GapReasons,
		}))
	}
	for _, capability := range inventory.ModelCapabilities {
		if capability.FreshnessState == providerinventory.FreshnessFresh &&
			capability.Confidence != providerinventory.ConfidenceStale &&
			capability.LifecycleState != providerinventory.LifecycleRemoved &&
			capability.AvailabilityState == providerinventory.AvailabilityAvailable {
			continue
		}
		scope := Scope{AdapterID: capability.AdapterID, ModelCapabilityID: capability.ModelCapabilityID, CanonicalModelID: capability.CanonicalModelID}
		reasons := []ReasonCode{ReasonModelUnavailable}
		if capability.FreshnessState == providerinventory.FreshnessStale || capability.FreshnessState == providerinventory.FreshnessExpired || capability.Confidence == providerinventory.ConfidenceStale {
			reasons = append(reasons, ReasonStaleEvidence)
		}
		out = append(out, normalizeObservation(Observation{
			ObservationKind: ObservationModelUnavailable,
			Scope:           scope,
			SourceRecordIDs: []string{capability.ModelCapabilityID},
			ObservedAt:      firstNonEmpty(capability.CapturedAt, formatTime(now)),
			FailureClass:    ReasonModelUnavailable,
			Confidence:      capability.Confidence,
			ReasonCodes:     reasons,
			GapReasons:      capability.GapReasons,
		}))
	}
	for _, snapshot := range inventory.QuotaSnapshots {
		scope := Scope{
			AdapterID:              snapshot.AdapterID,
			ProviderInstallationID: ptrValue(snapshot.ProviderInstallationID),
			AccountProfileID:       ptrValue(snapshot.AccountProfileID),
			ModelCapabilityID:      ptrValue(snapshot.ModelCapabilityID),
		}
		var kind ObservationKind
		var failure ReasonCode
		var reasons []ReasonCode
		switch {
		case strings.EqualFold(snapshot.TerminalErrorCode, "ErrRateLimited") || containsReason(snapshot.GapReasons, ReasonRateLimited429):
			kind = ObservationRateLimited
			failure = ReasonRateLimited429
			reasons = append(reasons, ReasonRateLimited429)
		case strings.EqualFold(snapshot.TerminalErrorCode, "ErrQuotaSnapshotMalformed") || containsReason(snapshot.GapReasons, ReasonMalformedResponse):
			kind = ObservationMalformedResponse
			failure = ReasonMalformedResponse
			reasons = append(reasons, ReasonMalformedResponse)
		case snapshot.Confidence == providerinventory.ConfidenceExact && snapshot.FreshnessState == providerinventory.FreshnessFresh && snapshot.RemainingValue != nil && *snapshot.RemainingValue <= 0:
			kind = ObservationQuotaExhausted
			failure = ReasonQuotaExhausted
			reasons = append(reasons, ReasonQuotaExhausted)
		case snapshot.Confidence == providerinventory.ConfidenceStale || snapshot.FreshnessState == providerinventory.FreshnessStale || snapshot.FreshnessState == providerinventory.FreshnessExpired:
			kind = ObservationProbeFailure
			failure = ReasonStaleEvidence
			reasons = append(reasons, ReasonStaleEvidence)
		}
		if kind == "" {
			continue
		}
		out = append(out, normalizeObservation(Observation{
			ObservationKind: kind,
			Scope:           scope,
			SourceRecordIDs: []string{snapshot.QuotaSnapshotID},
			ObservedAt:      firstNonEmpty(snapshot.CapturedAt, formatTime(now)),
			FailureClass:    failure,
			Confidence:      snapshot.Confidence,
			ReasonCodes:     reasons,
			GapReasons:      snapshot.GapReasons,
		}))
	}
	return out
}

func scoreScopes(inventory providerinventory.Report, summaries []budget.Summary) []Scope {
	seen := map[string]bool{}
	var scopes []Scope
	add := func(scope Scope) {
		scope = normalizeScope(scope)
		if scope.AdapterID == "" && scope.ModelCapabilityID == "" {
			return
		}
		key := scopeKey(scope)
		if seen[key] {
			return
		}
		seen[key] = true
		scopes = append(scopes, scope)
	}
	installationByAdapter := map[string]providerinventory.ProviderInstallation{}
	for _, installation := range inventory.Installations {
		if _, ok := installationByAdapter[installation.AdapterID]; !ok {
			installationByAdapter[installation.AdapterID] = installation
		}
	}
	for _, capability := range inventory.ModelCapabilities {
		scope := Scope{AdapterID: capability.AdapterID, ModelCapabilityID: capability.ModelCapabilityID, CanonicalModelID: capability.CanonicalModelID}
		if installation, ok := installationByAdapter[capability.AdapterID]; ok {
			scope.ProjectID = ptrValue(installation.ProjectID)
			scope.ProviderInstallationID = installation.ProviderInstallationID
		}
		add(scope)
	}
	if len(scopes) == 0 {
		for _, installation := range inventory.Installations {
			add(Scope{
				ProjectID:              ptrValue(installation.ProjectID),
				AdapterID:              installation.AdapterID,
				ProviderInstallationID: installation.ProviderInstallationID,
			})
		}
	}
	for _, summary := range summaries {
		add(Scope{
			ProjectID:         summary.Scope.ProjectID,
			DeliveryRunID:     summary.Scope.DeliveryRunID,
			TaskID:            summary.Scope.TaskID,
			AdapterID:         summary.Scope.AdapterID,
			AccountProfileID:  summary.Scope.AccountProfileID,
			ModelCapabilityID: summary.Scope.ModelCapabilityID,
		})
	}
	sort.Slice(scopes, func(i, j int) bool { return scopeKey(scopes[i]) < scopeKey(scopes[j]) })
	return scopes
}

func scoreScope(scope Scope, inputs Inputs, observations []Observation, policy Policy, now time.Time) Score {
	scope = normalizeScope(scope)
	scopeKey := scopeKey(scope)
	relevantObservations := filterObservations(scope, observations, policy.RecentFailureWindow, now)
	relevantBreakers := filterBreakers(scope, inputs.CircuitBreakers)
	auth := scoreAuth(scope, inputs.Inventory)
	model := scoreModel(scope, inputs.Inventory, policy)
	health := scoreHealth(scope, inputs.Inventory, relevantObservations)
	quota := scoreQuota(scope, inputs.Inventory.QuotaSnapshots, inputs.BudgetSummaries, policy)
	rateLimit := scoreRateLimit(relevantObservations, relevantBreakers)
	failures := scoreRecentFailures(relevantObservations, policy)
	components := []Component{auth, model, health, quota, rateLimit, failures}
	total := 0
	var hard []ReasonCode
	var gaps []string
	var evidence []string
	for _, component := range components {
		total += component.Score
		evidence = append(evidence, component.EvidenceRecordIDs...)
		gaps = append(gaps, stringReasons(component.ReasonCodes)...)
		if component.Hard {
			hard = append(hard, component.ReasonCodes...)
		}
	}
	for _, breaker := range relevantBreakers {
		evidence = append(evidence, breaker.CircuitBreakerID)
		if breaker.State == BreakerOpen {
			hard = append(hard, ReasonOpenBreaker, breaker.StateReason)
		}
	}
	if policy.ExactQuotaRequired && !hasReason(hard, ReasonQuotaExhausted) && !quotaExactEligible(quota) {
		hard = append(hard, ReasonQuotaConfidenceInsufficient)
	}
	hard = dedupeReasons(hard)
	evidence = dedupeStrings(evidence)
	observationIDs := observationIDs(relevantObservations)
	breakerIDs := breakerIDs(relevantBreakers)
	quotaIDs, usageIDs, policyIDs, reservationIDs := evidenceBuckets(scope, inputs)
	evidence = append(evidence, quotaIDs...)
	evidence = append(evidence, usageIDs...)
	evidence = append(evidence, policyIDs...)
	evidence = append(evidence, reservationIDs...)
	evidence = dedupeStrings(evidence)
	score := Score{
		SchemaVersion:         ScoreSchema,
		RecordVersion:         1,
		ScopeKey:              scopeKey,
		Scope:                 scope,
		Score:                 clampScore(total),
		Eligible:              len(hard) == 0,
		Components:            components,
		HardIneligibleReasons: hard,
		EvidenceRecordIDs:     evidence,
		ObservationIDs:        observationIDs,
		CircuitBreakerIDs:     breakerIDs,
		QuotaSnapshotIDs:      quotaIDs,
		UsageRecordIDs:        usageIDs,
		BudgetPolicyIDs:       policyIDs,
		BudgetReservationIDs:  reservationIDs,
		Heuristic:             true,
		PolicyVersion:         PolicyVersion,
		CapturedAt:            formatTime(now),
		GapReasons:            dedupeStrings(gaps),
	}
	score.ScoreConfidence = scoreConfidence(components)
	score.AvailabilityScoreID = scoreID(score)
	return normalizeScore(score)
}

func scoreAuth(scope Scope, inventory providerinventory.Report) Component {
	component := Component{Name: "auth", MaxScore: 25, Confidence: providerinventory.ConfidenceUnknown, FreshnessState: providerinventory.FreshnessNotApplicable, ReasonCodes: []ReasonCode{ReasonUnknownTelemetry}, Explanation: "auth readiness is unknown"}
	readiness, ok := latestAuth(scope, inventory.AuthReadiness)
	if !ok {
		component.Hard = true
		return component
	}
	component.EvidenceRecordIDs = []string{readiness.AuthReadinessID}
	component.Confidence = readiness.Confidence
	component.FreshnessState = readiness.FreshnessState
	if readiness.ReadinessState == providerinventory.ReadinessReady && readiness.FreshnessState == providerinventory.FreshnessFresh && readiness.Confidence != providerinventory.ConfidenceStale {
		component.Score = 25
		component.ReasonCodes = nil
		component.Explanation = "fresh auth readiness is ready"
		return component
	}
	component.Hard = true
	component.ReasonCodes = []ReasonCode{ReasonAuth}
	if readiness.FreshnessState == providerinventory.FreshnessStale || readiness.FreshnessState == providerinventory.FreshnessExpired || readiness.Confidence == providerinventory.ConfidenceStale {
		component.ReasonCodes = append(component.ReasonCodes, ReasonStaleEvidence)
	}
	component.Explanation = "auth readiness is not ready"
	return component
}

func scoreModel(scope Scope, inventory providerinventory.Report, policy Policy) Component {
	component := Component{Name: "model", MaxScore: 0, Confidence: providerinventory.ConfidenceUnknown, FreshnessState: providerinventory.FreshnessNotApplicable, ReasonCodes: []ReasonCode{ReasonMissingHardCapability}, Explanation: "model capability is unknown"}
	if scope.ModelCapabilityID == "" {
		component.Hard = true
		return component
	}
	for _, capability := range inventory.ModelCapabilities {
		if capability.ModelCapabilityID != scope.ModelCapabilityID {
			continue
		}
		component.EvidenceRecordIDs = []string{capability.ModelCapabilityID}
		component.Confidence = capability.Confidence
		component.FreshnessState = capability.FreshnessState
		if capability.SatisfiesHardRequirements(policy.HardRequirement) {
			component.ReasonCodes = nil
			component.Explanation = "fresh model capability satisfies hard requirements"
			return component
		}
		component.Hard = true
		component.ReasonCodes = []ReasonCode{ReasonModelUnavailable}
		if capability.FreshnessState == providerinventory.FreshnessStale || capability.FreshnessState == providerinventory.FreshnessExpired || capability.Confidence == providerinventory.ConfidenceStale {
			component.ReasonCodes = append(component.ReasonCodes, ReasonStaleEvidence)
		}
		if capability.AvailabilityState == providerinventory.AvailabilityUnknown || capability.Confidence == providerinventory.ConfidenceUnknown {
			component.ReasonCodes = append(component.ReasonCodes, ReasonMissingHardCapability)
		}
		component.Explanation = "model capability cannot satisfy hard requirements"
		return component
	}
	component.Hard = true
	return component
}

func scoreHealth(scope Scope, inventory providerinventory.Report, observations []Observation) Component {
	component := Component{Name: "health", MaxScore: 25, Confidence: providerinventory.ConfidenceEstimated, FreshnessState: providerinventory.FreshnessFresh, Score: 10, Explanation: "no recent successful health probe, but no blocking health failure"}
	for _, observation := range observations {
		if observation.ObservationKind == ObservationProviderOutage || observation.ObservationKind == ObservationMalformedResponse {
			component.Score = 0
			component.Hard = observation.ObservationKind == ObservationProviderOutage
			component.Confidence = observation.Confidence
			component.ReasonCodes = append(component.ReasonCodes, observation.FailureClass)
			component.EvidenceRecordIDs = append(component.EvidenceRecordIDs, observation.AvailabilityObservationID)
			component.Explanation = "recent provider health failure is present"
			return component
		}
	}
	for _, installation := range inventory.Installations {
		if !scopeMatchesInstallation(scope, installation) {
			continue
		}
		component.EvidenceRecordIDs = append(component.EvidenceRecordIDs, installation.ProviderInstallationID)
		component.Confidence = installation.Confidence
		component.FreshnessState = installation.FreshnessState
		if installation.InstallationState == providerinventory.InstallationInstalled && installation.UsableForInvocation == "yes" && installation.FreshnessState == providerinventory.FreshnessFresh {
			component.Score = 25
			component.Explanation = "fresh installation probe reports usable invocation"
			return component
		}
		component.Score = 0
		component.Hard = true
		component.ReasonCodes = append(component.ReasonCodes, ReasonInstallationUnavailable)
		component.Explanation = "installation is missing, stale, or unusable"
		return component
	}
	component.Hard = true
	component.Score = 0
	component.ReasonCodes = []ReasonCode{ReasonInstallationUnavailable}
	component.Explanation = "no installation health evidence is present"
	return component
}

func scoreQuota(scope Scope, snapshots []providerinventory.QuotaSnapshot, budgets []budget.Summary, policy Policy) Component {
	component := Component{Name: "quota", MaxScore: 20, Confidence: providerinventory.ConfidenceUnknown, FreshnessState: providerinventory.FreshnessNotApplicable, Explanation: "quota telemetry is unknown", ReasonCodes: []ReasonCode{ReasonUnknownTelemetry}}
	var best *providerinventory.QuotaSnapshot
	for i := range snapshots {
		snapshot := snapshots[i]
		if !scopeMatchesQuota(scope, snapshot) {
			continue
		}
		component.EvidenceRecordIDs = append(component.EvidenceRecordIDs, snapshot.QuotaSnapshotID)
		if best == nil || snapshot.CapturedAt > best.CapturedAt || (snapshot.CapturedAt == best.CapturedAt && snapshot.QuotaSnapshotID > best.QuotaSnapshotID) {
			best = &snapshots[i]
		}
	}
	if best != nil {
		component.Confidence = best.Confidence
		component.FreshnessState = best.FreshnessState
		switch {
		case len(best.ConflictSet) > 0:
			component.Score = 0
			component.Hard = policy.ExactQuotaRequired
			component.ReasonCodes = []ReasonCode{ReasonQuotaConfidenceInsufficient}
			component.Explanation = "quota snapshots conflict"
			return component
		case best.Confidence == providerinventory.ConfidenceExact && best.FreshnessState == providerinventory.FreshnessFresh && best.RemainingValue != nil && *best.RemainingValue > 0:
			component.Score = 20
			component.ReasonCodes = nil
			component.Explanation = "fresh exact quota reports remaining capacity"
			return component
		case best.Confidence == providerinventory.ConfidenceExact && best.RemainingValue != nil && *best.RemainingValue <= 0:
			component.Score = 0
			component.Hard = true
			component.ReasonCodes = []ReasonCode{ReasonQuotaExhausted}
			component.Explanation = "fresh exact quota is exhausted"
			return component
		case best.Confidence == providerinventory.ConfidenceStale || best.FreshnessState == providerinventory.FreshnessStale || best.FreshnessState == providerinventory.FreshnessExpired:
			component.ReasonCodes = []ReasonCode{ReasonStaleEvidence}
		default:
			component.ReasonCodes = []ReasonCode{ReasonQuotaConfidenceInsufficient}
		}
	}
	if policy.ConservativeLocalScheduling && budgetAvailable(scope, budgets) {
		component.Score = 10
		component.Confidence = providerinventory.ConfidenceEstimated
		component.ReasonCodes = []ReasonCode{ReasonQuotaConfidenceInsufficient}
		component.Explanation = "policy allows conservative local scheduling without exact provider quota"
		return component
	}
	component.Hard = policy.ExactQuotaRequired
	if policy.ExactQuotaRequired && !hasReason(component.ReasonCodes, ReasonStaleEvidence) {
		component.ReasonCodes = []ReasonCode{ReasonQuotaConfidenceInsufficient}
	}
	return component
}

func scoreRateLimit(observations []Observation, breakers []CircuitBreaker) Component {
	component := Component{Name: "rate_limit", MaxScore: 15, Score: 15, Confidence: providerinventory.ConfidenceEstimated, FreshnessState: providerinventory.FreshnessFresh, Explanation: "no active rate-limit evidence"}
	for _, breaker := range breakers {
		if breaker.BreakerKind == BreakerRateLimit && breaker.State == BreakerOpen {
			component.Score = 0
			component.Hard = true
			component.ReasonCodes = []ReasonCode{ReasonRateLimited429, ReasonOpenBreaker}
			component.EvidenceRecordIDs = append(component.EvidenceRecordIDs, breaker.CircuitBreakerID)
			component.Explanation = "rate-limit breaker is open"
			return component
		}
	}
	for _, observation := range observations {
		if observation.ObservationKind == ObservationRateLimited {
			component.Score = 0
			component.Hard = true
			component.Confidence = observation.Confidence
			component.ReasonCodes = []ReasonCode{ReasonRateLimited429}
			component.EvidenceRecordIDs = append(component.EvidenceRecordIDs, observation.AvailabilityObservationID)
			component.Explanation = "recent provider rate-limit evidence is present"
			return component
		}
	}
	return component
}

func scoreRecentFailures(observations []Observation, policy Policy) Component {
	component := Component{Name: "recent_failures", MaxScore: 15, Score: 15, Confidence: providerinventory.ConfidenceEstimated, FreshnessState: providerinventory.FreshnessFresh, Explanation: "no relevant recent failures"}
	failures := 0
	for _, observation := range observations {
		switch observation.ObservationKind {
		case ObservationProbeFailure, ObservationTransportFailure, ObservationProviderOutage, ObservationMalformedResponse, ObservationLaunchFailure:
			failures++
			component.EvidenceRecordIDs = append(component.EvidenceRecordIDs, observation.AvailabilityObservationID)
			if observation.FailureClass != "" {
				component.ReasonCodes = append(component.ReasonCodes, observation.FailureClass)
			}
		}
	}
	component.ReasonCodes = dedupeReasons(component.ReasonCodes)
	switch {
	case failures == 0:
		return component
	case failures >= policy.FailureThreshold:
		component.Score = 0
		component.Hard = true
		component.Explanation = "recent failure threshold is met"
	default:
		component.Score = 8
		component.Explanation = "recent transient failures are below threshold"
	}
	return component
}

func insertObservation(ctx context.Context, tx storage.Tx, observation Observation) error {
	observation = normalizeObservation(observation)
	if err := ValidateObservation(observation); err != nil {
		return err
	}
	payload, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	sourceIDs, err := json.Marshal(observation.SourceRecordIDs)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT OR IGNORE INTO availability_observations(
		availability_observation_id, observation_kind, scope_key, adapter_id, provider_installation_id,
		account_profile_id, model_capability_id, failure_class, observed_at, confidence, source_record_ids_json, payload_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		observation.AvailabilityObservationID, string(observation.ObservationKind), observation.ScopeKey,
		observation.Scope.AdapterID, observation.Scope.ProviderInstallationID, observation.Scope.AccountProfileID,
		observation.Scope.ModelCapabilityID, string(observation.FailureClass), observation.ObservedAt, string(observation.Confidence),
		string(sourceIDs), string(payload))
	return err
}

func insertScore(ctx context.Context, tx storage.Tx, score Score) error {
	score = normalizeScore(score)
	if err := ValidateScore(score); err != nil {
		return err
	}
	payload, err := json.Marshal(score)
	if err != nil {
		return err
	}
	hard, err := json.Marshal(score.HardIneligibleReasons)
	if err != nil {
		return err
	}
	evidence, err := json.Marshal(score.EvidenceRecordIDs)
	if err != nil {
		return err
	}
	eligible := 0
	if score.Eligible {
		eligible = 1
	}
	_, err = tx.Exec(ctx, `INSERT OR IGNORE INTO availability_scores(
		availability_score_id, scope_key, adapter_id, provider_installation_id, account_profile_id,
		model_capability_id, score, eligible, score_confidence, hard_ineligible_reasons_json,
		evidence_record_ids_json, captured_at, policy_version, payload_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		score.AvailabilityScoreID, score.ScopeKey, score.Scope.AdapterID, score.Scope.ProviderInstallationID,
		score.Scope.AccountProfileID, score.Scope.ModelCapabilityID, score.Score, eligible, string(score.ScoreConfidence),
		string(hard), string(evidence), score.CapturedAt, score.PolicyVersion, string(payload))
	return err
}

func ValidateObservation(observation Observation) error {
	if observation.SchemaVersion != ObservationSchema || !strings.HasPrefix(observation.AvailabilityObservationID, "avobs_") {
		return fmt.Errorf("%w: invalid observation identity", ErrAvailabilityRecord)
	}
	if !knownObservationKind(observation.ObservationKind) || observation.ScopeKey == "" || len(observation.SourceRecordIDs) == 0 {
		return fmt.Errorf("%w: observation kind, scope, and source records are required", ErrAvailabilityRecord)
	}
	if _, err := time.Parse(time.RFC3339Nano, observation.ObservedAt); err != nil {
		return fmt.Errorf("%w: observed_at must be RFC3339", ErrAvailabilityRecord)
	}
	return nil
}

func ValidateScore(score Score) error {
	if score.SchemaVersion != ScoreSchema || !strings.HasPrefix(score.AvailabilityScoreID, "avscore_") {
		return fmt.Errorf("%w: invalid score identity", ErrAvailabilityRecord)
	}
	if score.ScopeKey == "" || score.Score < 0 || score.Score > 100 || len(score.Components) == 0 || score.PolicyVersion == "" {
		return fmt.Errorf("%w: score scope, components, score, and policy version are required", ErrAvailabilityRecord)
	}
	if score.Eligible && len(score.HardIneligibleReasons) > 0 {
		return fmt.Errorf("%w: eligible score cannot contain hard ineligible reasons", ErrAvailabilityRecord)
	}
	if !score.Eligible && len(score.HardIneligibleReasons) == 0 {
		return fmt.Errorf("%w: ineligible score must explain hard reasons", ErrAvailabilityRecord)
	}
	return nil
}

func normalizeObservation(observation Observation) Observation {
	observation.Scope = normalizeScope(observation.Scope)
	if observation.SchemaVersion == "" {
		observation.SchemaVersion = ObservationSchema
	}
	if observation.RecordVersion == 0 {
		observation.RecordVersion = 1
	}
	if observation.ScopeKey == "" {
		observation.ScopeKey = scopeKey(observation.Scope)
	}
	if observation.ObservedAt == "" {
		observation.ObservedAt = "1970-01-01T00:00:00Z"
	}
	if observation.Confidence == "" {
		observation.Confidence = providerinventory.ConfidenceUnknown
	}
	observation.SourceRecordIDs = dedupeStrings(observation.SourceRecordIDs)
	observation.ReasonCodes = dedupeReasons(observation.ReasonCodes)
	observation.GapReasons = dedupeStrings(observation.GapReasons)
	if observation.AvailabilityObservationID == "" {
		observation.AvailabilityObservationID = observationID(observation)
	}
	return observation
}

func normalizeScore(score Score) Score {
	score.Scope = normalizeScope(score.Scope)
	if score.SchemaVersion == "" {
		score.SchemaVersion = ScoreSchema
	}
	if score.RecordVersion == 0 {
		score.RecordVersion = 1
	}
	if score.ScopeKey == "" {
		score.ScopeKey = scopeKey(score.Scope)
	}
	if score.PolicyVersion == "" {
		score.PolicyVersion = PolicyVersion
	}
	if score.CapturedAt == "" {
		score.CapturedAt = "1970-01-01T00:00:00Z"
	}
	score.HardIneligibleReasons = dedupeReasons(score.HardIneligibleReasons)
	score.EvidenceRecordIDs = dedupeStrings(score.EvidenceRecordIDs)
	score.ObservationIDs = dedupeStrings(score.ObservationIDs)
	score.CircuitBreakerIDs = dedupeStrings(score.CircuitBreakerIDs)
	score.QuotaSnapshotIDs = dedupeStrings(score.QuotaSnapshotIDs)
	score.UsageRecordIDs = dedupeStrings(score.UsageRecordIDs)
	score.BudgetPolicyIDs = dedupeStrings(score.BudgetPolicyIDs)
	score.BudgetReservationIDs = dedupeStrings(score.BudgetReservationIDs)
	score.GapReasons = dedupeStrings(score.GapReasons)
	if score.ScoreConfidence == "" {
		score.ScoreConfidence = scoreConfidence(score.Components)
	}
	if score.AvailabilityScoreID == "" {
		score.AvailabilityScoreID = scoreID(score)
	}
	return score
}

func normalizePolicy(policy Policy) Policy {
	if policy.RecentFailureWindow <= 0 {
		policy.RecentFailureWindow = 30 * time.Minute
	}
	if policy.FailureThreshold <= 0 {
		policy.FailureThreshold = 3
	}
	return policy
}

func normalizeScope(scope Scope) Scope {
	scope.ProjectID = strings.TrimSpace(scope.ProjectID)
	scope.DeliveryRunID = strings.TrimSpace(scope.DeliveryRunID)
	scope.TaskID = strings.TrimSpace(scope.TaskID)
	scope.AdapterID = strings.TrimSpace(scope.AdapterID)
	scope.ProviderInstallationID = strings.TrimSpace(scope.ProviderInstallationID)
	scope.AccountProfileID = strings.TrimSpace(scope.AccountProfileID)
	scope.ModelCapabilityID = strings.TrimSpace(scope.ModelCapabilityID)
	scope.CanonicalModelID = strings.TrimSpace(scope.CanonicalModelID)
	return scope
}

func scopeKey(scope Scope) string {
	scope = normalizeScope(scope)
	data, _ := json.Marshal(scope)
	return string(data)
}

func observationID(observation Observation) string {
	type stableObservation struct {
		Kind       ObservationKind              `json:"kind"`
		ScopeKey   string                       `json:"scope_key"`
		Sources    []string                     `json:"sources"`
		Observed   string                       `json:"observed_at"`
		Failure    ReasonCode                   `json:"failure_class,omitempty"`
		Confidence providerinventory.Confidence `json:"confidence"`
		Reasons    []ReasonCode                 `json:"reasons"`
	}
	payload := stableObservation{
		Kind: observation.ObservationKind, ScopeKey: observation.ScopeKey, Sources: observation.SourceRecordIDs,
		Observed: observation.ObservedAt, Failure: observation.FailureClass, Confidence: observation.Confidence, Reasons: observation.ReasonCodes,
	}
	return "avobs_" + hashBase32(mustJSON(payload))[:26]
}

func scoreID(score Score) string {
	type stableScore struct {
		ScopeKey  string       `json:"scope_key"`
		Score     int          `json:"score"`
		Eligible  bool         `json:"eligible"`
		Hard      []ReasonCode `json:"hard"`
		Evidence  []string     `json:"evidence"`
		Policy    string       `json:"policy"`
		Captured  string       `json:"captured_at"`
		Component []Component  `json:"components"`
	}
	payload := stableScore{
		ScopeKey: score.ScopeKey, Score: score.Score, Eligible: score.Eligible, Hard: score.HardIneligibleReasons,
		Evidence: score.EvidenceRecordIDs, Policy: score.PolicyVersion, Captured: score.CapturedAt, Component: score.Components,
	}
	return "avscore_" + hashBase32(mustJSON(payload))[:26]
}

func hashBase32(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	return strings.ToLower(enc)
}

func hashHex(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "json-error:" + err.Error()
	}
	return string(data)
}

func latestAuth(scope Scope, records []providerinventory.AuthReadiness) (providerinventory.AuthReadiness, bool) {
	var best providerinventory.AuthReadiness
	found := false
	for _, record := range records {
		if !scopeMatchesAuth(scope, record) {
			continue
		}
		if !found || record.CapturedAt > best.CapturedAt || (record.CapturedAt == best.CapturedAt && record.AuthReadinessID > best.AuthReadinessID) {
			best = record
			found = true
		}
	}
	return best, found
}

func scopeMatchesAuth(scope Scope, readiness providerinventory.AuthReadiness) bool {
	if scope.AdapterID != "" && readiness.AdapterID != scope.AdapterID {
		return false
	}
	if scope.ProviderInstallationID != "" && ptrValue(readiness.ProviderInstallationID) != "" && ptrValue(readiness.ProviderInstallationID) != scope.ProviderInstallationID {
		return false
	}
	if scope.AccountProfileID != "" && ptrValue(readiness.AccountProfileID) != "" && ptrValue(readiness.AccountProfileID) != scope.AccountProfileID {
		return false
	}
	return true
}

func scopeMatchesInstallation(scope Scope, installation providerinventory.ProviderInstallation) bool {
	if scope.AdapterID != "" && installation.AdapterID != scope.AdapterID {
		return false
	}
	if scope.ProviderInstallationID != "" && installation.ProviderInstallationID != scope.ProviderInstallationID {
		return false
	}
	return true
}

func scopeMatchesQuota(scope Scope, snapshot providerinventory.QuotaSnapshot) bool {
	if scope.AdapterID != "" && snapshot.AdapterID != "" && snapshot.AdapterID != scope.AdapterID {
		return false
	}
	if scope.ProviderInstallationID != "" && ptrValue(snapshot.ProviderInstallationID) != "" && ptrValue(snapshot.ProviderInstallationID) != scope.ProviderInstallationID {
		return false
	}
	if scope.AccountProfileID != "" && ptrValue(snapshot.AccountProfileID) != "" && ptrValue(snapshot.AccountProfileID) != scope.AccountProfileID {
		return false
	}
	if scope.ModelCapabilityID != "" && ptrValue(snapshot.ModelCapabilityID) != "" && ptrValue(snapshot.ModelCapabilityID) != scope.ModelCapabilityID {
		return false
	}
	return true
}

func scopeMatchesBudget(scope Scope, summary budget.Summary) bool {
	if scope.ProjectID != "" && summary.Scope.ProjectID != "" && summary.Scope.ProjectID != scope.ProjectID {
		return false
	}
	if scope.AdapterID != "" && summary.Scope.AdapterID != "" && summary.Scope.AdapterID != scope.AdapterID {
		return false
	}
	if scope.AccountProfileID != "" && summary.Scope.AccountProfileID != "" && summary.Scope.AccountProfileID != scope.AccountProfileID {
		return false
	}
	if scope.ModelCapabilityID != "" && summary.Scope.ModelCapabilityID != "" && summary.Scope.ModelCapabilityID != scope.ModelCapabilityID {
		return false
	}
	return true
}

func scopeMatchesObservation(scope Scope, observation Observation) bool {
	if scope.AdapterID != "" && observation.Scope.AdapterID != "" && observation.Scope.AdapterID != scope.AdapterID {
		return false
	}
	if scope.ProviderInstallationID != "" && observation.Scope.ProviderInstallationID != "" && observation.Scope.ProviderInstallationID != scope.ProviderInstallationID {
		return false
	}
	if scope.AccountProfileID != "" && observation.Scope.AccountProfileID != "" && observation.Scope.AccountProfileID != scope.AccountProfileID {
		return false
	}
	if scope.ModelCapabilityID != "" && observation.Scope.ModelCapabilityID != "" && observation.Scope.ModelCapabilityID != scope.ModelCapabilityID {
		return false
	}
	return true
}

func scopeMatchesBreaker(scope Scope, breaker CircuitBreaker) bool {
	if breaker.Scope.AdapterID != "" && scope.AdapterID != "" && breaker.Scope.AdapterID != scope.AdapterID {
		return false
	}
	if breaker.Scope.ModelCapabilityID != "" && scope.ModelCapabilityID != "" && breaker.Scope.ModelCapabilityID != scope.ModelCapabilityID {
		return false
	}
	if breaker.ScopeKey != "" && breaker.ScopeKey == scopeKey(scope) {
		return true
	}
	return breaker.Scope.AdapterID == scope.AdapterID || breaker.Scope.ModelCapabilityID == scope.ModelCapabilityID
}

func filterObservations(scope Scope, observations []Observation, window time.Duration, now time.Time) []Observation {
	var out []Observation
	cutoff := now.Add(-window)
	for _, observation := range observations {
		observation = normalizeObservation(observation)
		if !scopeMatchesObservation(scope, observation) {
			continue
		}
		observed, err := time.Parse(time.RFC3339Nano, observation.ObservedAt)
		if err == nil && observed.Before(cutoff) {
			continue
		}
		out = append(out, observation)
	}
	sortObservations(out)
	return out
}

func filterBreakers(scope Scope, breakers []CircuitBreaker) []CircuitBreaker {
	var out []CircuitBreaker
	for _, breaker := range breakers {
		if scopeMatchesBreaker(scope, breaker) {
			out = append(out, breaker)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CircuitBreakerID < out[j].CircuitBreakerID })
	return out
}

func evidenceBuckets(scope Scope, inputs Inputs) ([]string, []string, []string, []string) {
	var quotaIDs, usageIDs, policyIDs, reservationIDs []string
	for _, snapshot := range inputs.Inventory.QuotaSnapshots {
		if scopeMatchesQuota(scope, snapshot) {
			quotaIDs = append(quotaIDs, snapshot.QuotaSnapshotID)
		}
	}
	for _, summary := range inputs.UsageSummaries {
		if scope.AdapterID != "" && summary.AdapterID != "" && summary.AdapterID != scope.AdapterID {
			continue
		}
		if scope.ModelCapabilityID != "" && summary.ModelCapabilityID != "" && summary.ModelCapabilityID != scope.ModelCapabilityID {
			continue
		}
		usageIDs = append(usageIDs, summary.UsageRecordIDs...)
	}
	for _, summary := range inputs.BudgetSummaries {
		if scopeMatchesBudget(scope, summary) {
			policyIDs = append(policyIDs, summary.BudgetPolicyID)
			reservationIDs = append(reservationIDs, summary.ActiveReservationIDs...)
		}
	}
	return dedupeStrings(quotaIDs), dedupeStrings(usageIDs), dedupeStrings(policyIDs), dedupeStrings(reservationIDs)
}

func budgetAvailable(scope Scope, summaries []budget.Summary) bool {
	for _, summary := range summaries {
		if scopeMatchesBudget(scope, summary) && summary.PolicyMode == budget.PolicyHard && summary.AvailableValue > 0 && summary.Denial == "" {
			return true
		}
	}
	return false
}

func quotaExactEligible(component Component) bool {
	return component.Name == "quota" && component.Score == 20 && !component.Hard
}

func scoreConfidence(components []Component) providerinventory.Confidence {
	hasStale := false
	hasUnknown := false
	hasEstimated := false
	for _, component := range components {
		switch component.Confidence {
		case providerinventory.ConfidenceStale:
			hasStale = true
		case providerinventory.ConfidenceUnknown, providerinventory.ConfidenceUnavailable:
			hasUnknown = true
		case providerinventory.ConfidenceEstimated:
			hasEstimated = true
		}
		if component.FreshnessState == providerinventory.FreshnessStale || component.FreshnessState == providerinventory.FreshnessExpired {
			hasStale = true
		}
	}
	switch {
	case hasStale:
		return providerinventory.ConfidenceStale
	case hasUnknown:
		return providerinventory.ConfidenceUnknown
	case hasEstimated:
		return providerinventory.ConfidenceEstimated
	default:
		return providerinventory.ConfidenceExact
	}
}

func dedupeObservations(observations []Observation) []Observation {
	seen := map[string]bool{}
	var out []Observation
	for _, observation := range observations {
		observation = normalizeObservation(observation)
		if seen[observation.AvailabilityObservationID] {
			continue
		}
		seen[observation.AvailabilityObservationID] = true
		out = append(out, observation)
	}
	return out
}

func sortObservations(observations []Observation) {
	sort.Slice(observations, func(i, j int) bool {
		if observations[i].ObservedAt != observations[j].ObservedAt {
			return observations[i].ObservedAt < observations[j].ObservedAt
		}
		return observations[i].AvailabilityObservationID < observations[j].AvailabilityObservationID
	})
}

func observationIDs(observations []Observation) []string {
	ids := make([]string, 0, len(observations))
	for _, observation := range observations {
		ids = append(ids, observation.AvailabilityObservationID)
	}
	return dedupeStrings(ids)
}

func breakerIDs(breakers []CircuitBreaker) []string {
	ids := make([]string, 0, len(breakers))
	for _, breaker := range breakers {
		ids = append(ids, breaker.CircuitBreakerID)
	}
	return dedupeStrings(ids)
}

func knownObservationKind(kind ObservationKind) bool {
	switch kind {
	case ObservationProbeSuccess, ObservationProbeFailure, ObservationAuthFailure, ObservationQuotaExhausted,
		ObservationRateLimited, ObservationModelUnavailable, ObservationTransportFailure, ObservationProviderOutage,
		ObservationMalformedResponse, ObservationLaunchSuccess, ObservationLaunchFailure:
		return true
	default:
		return false
	}
}

func containsReason(values []string, reason ReasonCode) bool {
	for _, value := range values {
		if NormalizeReasonCode(value) == reason {
			return true
		}
	}
	return false
}

func hasReason(values []ReasonCode, reason ReasonCode) bool {
	for _, value := range values {
		if value == reason {
			return true
		}
	}
	return false
}

func stringReasons(values []ReasonCode) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			out = append(out, string(value))
		}
	}
	return out
}

func joinReasons(values []ReasonCode) string {
	if len(values) == 0 {
		return "none"
	}
	text := stringReasons(values)
	sort.Strings(text)
	return strings.Join(text, ",")
}

func dedupeReasons(values []ReasonCode) []ReasonCode {
	seen := map[ReasonCode]bool{}
	var out []ReasonCode
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func dedupeStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func ptrValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "1970-01-01T00:00:00Z"
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func clampScore(score int) int {
	if score < 0 {
		return 0
	}
	if score > 100 {
		return 100
	}
	return score
}

func DebugFingerprint(result Result) string {
	data, _ := json.Marshal(result)
	return "sha256:" + hashHex(string(data))
}

func ReasonCount(score Score, reason ReasonCode) int {
	count := 0
	for _, value := range score.HardIneligibleReasons {
		if value == reason {
			count++
		}
	}
	for _, component := range score.Components {
		for _, value := range component.ReasonCodes {
			if value == reason {
				count++
			}
		}
	}
	return count
}

func ComponentScore(score Score, name string) (int, bool) {
	for _, component := range score.Components {
		if component.Name == name {
			return component.Score, true
		}
	}
	return 0, false
}

func ParseRetryAfter(value string, now time.Time) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return formatTime(t)
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 0 {
		return ""
	}
	return formatTime(now.UTC().Add(time.Duration(seconds) * time.Second))
}
