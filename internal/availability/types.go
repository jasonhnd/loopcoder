// Package availability derives deterministic provider availability read models.
package availability

import (
	"errors"
	"time"

	"github.com/jasonhnd/loopcoder/internal/budget"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
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
	ReasonCooldownElapsed             ReasonCode = "cooldown-elapsed"
	ReasonRecoverySucceeded           ReasonCode = "recovery-succeeded"
	ReasonProbeLeaseActive            ReasonCode = "probe-lease-active"
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
	SchemaVersion        string       `json:"schema_version"`
	CircuitBreakerID     string       `json:"circuit_breaker_id"`
	BreakerKind          BreakerKind  `json:"breaker_kind"`
	State                BreakerState `json:"state"`
	ScopeKey             string       `json:"scope_key"`
	Scope                Scope        `json:"scope"`
	OpenedAt             string       `json:"opened_at,omitempty"`
	OpenUntil            string       `json:"open_until,omitempty"`
	HalfOpenProbeBudget  int          `json:"half_open_probe_budget"`
	HalfOpenProbeCount   int          `json:"half_open_probe_count"`
	FailureCount         int          `json:"failure_count"`
	SuccessCount         int          `json:"success_count"`
	LastObservationID    string       `json:"last_observation_id,omitempty"`
	LastObservationAt    string       `json:"last_observation_at,omitempty"`
	StateReason          ReasonCode   `json:"state_reason"`
	PolicyVersion        string       `json:"policy_version"`
	RecordVersion        int          `json:"record_version"`
	ProbeLeaseOwner      string       `json:"probe_lease_owner,omitempty"`
	ProbeLeaseExpiresAt  string       `json:"probe_lease_expires_at,omitempty"`
	ProbeLeaseGeneration int          `json:"probe_lease_generation,omitempty"`
}

type Policy struct {
	ExactQuotaRequired          bool
	ConservativeLocalScheduling bool
	RecentFailureWindow         time.Duration
	FailureThreshold            int
	HardRequirement             providerinventory.HardRequirement
	BaseCooldown                time.Duration
	MaxCooldown                 time.Duration
	Jitter                      time.Duration
	RandomSeed                  string
	HalfOpenProbeBudget         int
	RequiredSuccessCount        int
	ProbeLeaseDuration          time.Duration
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
