package availability

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

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
