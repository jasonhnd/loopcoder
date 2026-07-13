package availability

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

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
