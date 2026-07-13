package availability

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

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
	breakers := deriveCircuitBreakers(inputs.CircuitBreakers, observations, policy, now)
	scoringInputs := inputs
	scoringInputs.CircuitBreakers = breakers

	scopes := scoreScopes(inputs.Inventory, inputs.BudgetSummaries)
	scores := make([]Score, 0, len(scopes))
	for _, scope := range scopes {
		score := scoreScope(scope, scoringInputs, observations, policy, now)
		scores = append(scores, score)
	}
	sort.Slice(scores, func(i, j int) bool {
		if scores[i].Scope.AdapterID != scores[j].Scope.AdapterID {
			return scores[i].Scope.AdapterID < scores[j].Scope.AdapterID
		}
		return scores[i].ScopeKey < scores[j].ScopeKey
	})
	return Result{Observations: observations, Scores: scores, CircuitBreakers: breakers}
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
