package availability

import (
	"sort"
	"time"
)

type breakerEvent string

const (
	eventSuccess          breakerEvent = "success"
	eventTransientFailure breakerEvent = "transient-failure"
	eventProviderOutage   breakerEvent = "provider-outage"
	eventRateLimit        breakerEvent = "rate-limit"
	eventQuotaExhausted   breakerEvent = "quota-exhausted"
	eventAuthFailure      breakerEvent = "auth-failure"
	eventModelUnavailable breakerEvent = "model-unavailable"
	eventMalformed        breakerEvent = "malformed-response"
	eventProbeFailure     breakerEvent = "probe-failure"
)

func deriveCircuitBreakers(existing []CircuitBreaker, observations []Observation, policy Policy, now time.Time) []CircuitBreaker {
	breakers := map[string]CircuitBreaker{}
	for _, breaker := range existing {
		breaker = normalizeBreaker(breaker)
		breaker = advanceBreakerCooldown(breaker, policy, now)
		breakers[breaker.CircuitBreakerID] = breaker
	}

	for _, observation := range observations {
		observation = normalizeObservation(observation)
		if isSuccessObservation(observation) {
			for id, breaker := range breakers {
				if scopeMatchesObservation(breaker.Scope, observation) {
					breakers[id] = applyBreakerEvent(breaker, observation, eventSuccess, policy, now)
				}
			}
			continue
		}
		kind, event, reason, ok := breakerEventForObservation(observation)
		if !ok {
			continue
		}
		scope := breakerScope(kind, observation.Scope)
		id := breakerID(kind, scope)
		breaker, ok := breakers[id]
		if !ok {
			breaker = normalizeBreaker(CircuitBreaker{
				BreakerKind: kind,
				State:       BreakerClosed,
				Scope:       scope,
				StateReason: reason,
			})
		}
		breakers[id] = applyBreakerEvent(breaker, observation, event, policy, now)
	}

	out := make([]CircuitBreaker, 0, len(breakers))
	for _, breaker := range breakers {
		out = append(out, normalizeBreaker(breaker))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CircuitBreakerID < out[j].CircuitBreakerID })
	return out
}

func applyBreakerEvent(breaker CircuitBreaker, observation Observation, event breakerEvent, policy Policy, now time.Time) CircuitBreaker {
	breaker = normalizeBreaker(breaker)
	if observationAlreadyApplied(breaker, observation) {
		return breaker
	}
	before := breaker
	observedAt := parseTimeOr(observation.ObservedAt, now)
	reason := observation.FailureClass
	if reason == "" && len(observation.ReasonCodes) > 0 {
		reason = observation.ReasonCodes[0]
	}
	if reason == "" {
		reason = ReasonUnknownTelemetry
	}

	switch event {
	case eventSuccess:
		switch breaker.State {
		case BreakerClosed:
			breaker.FailureCount = 0
			breaker.SuccessCount++
			breaker.StateReason = ReasonRecoverySucceeded
		case BreakerHalfOpen:
			breaker.SuccessCount++
			breaker.StateReason = ReasonRecoverySucceeded
			if breaker.SuccessCount >= policy.RequiredSuccessCount {
				breaker.State = BreakerClosed
				breaker.OpenedAt = ""
				breaker.OpenUntil = ""
				breaker.HalfOpenProbeCount = 0
				breaker.FailureCount = 0
				breaker.ProbeLeaseOwner = ""
				breaker.ProbeLeaseExpiresAt = ""
			}
		}
	case eventTransientFailure, eventMalformed, eventProbeFailure:
		breaker.FailureCount++
		breaker.SuccessCount = 0
		breaker.StateReason = reason
		threshold := policy.FailureThreshold
		if event == eventMalformed {
			threshold = policy.FailureThreshold
		}
		if breaker.State == BreakerHalfOpen || breaker.FailureCount >= threshold {
			breaker = openBreaker(breaker, observation, policy, observedAt, reason)
		}
	case eventProviderOutage, eventRateLimit, eventQuotaExhausted, eventAuthFailure, eventModelUnavailable:
		breaker.FailureCount++
		breaker.SuccessCount = 0
		breaker = openBreaker(breaker, observation, policy, observedAt, reason)
	}

	breaker.LastObservationID = observation.AvailabilityObservationID
	breaker.LastObservationAt = observation.ObservedAt
	if breakerChanged(before, breaker) {
		breaker.RecordVersion = before.RecordVersion + 1
	}
	return normalizeBreaker(breaker)
}

func openBreaker(breaker CircuitBreaker, observation Observation, policy Policy, observedAt time.Time, reason ReasonCode) CircuitBreaker {
	if breaker.OpenedAt == "" || breaker.State != BreakerOpen {
		breaker.OpenedAt = formatTime(observedAt)
	}
	breaker.State = BreakerOpen
	breaker.StateReason = reason
	breaker.HalfOpenProbeBudget = policy.HalfOpenProbeBudget
	breaker.HalfOpenProbeCount = 0
	breaker.SuccessCount = 0
	breaker.ProbeLeaseOwner = ""
	breaker.ProbeLeaseExpiresAt = ""
	candidate := observationCooldownUntil(observation, breaker, policy, observedAt)
	if current := parseOptionalTime(breaker.OpenUntil); current != nil && current.After(candidate) {
		candidate = *current
	}
	breaker.OpenUntil = formatTime(candidate)
	return breaker
}

func advanceBreakerCooldown(breaker CircuitBreaker, policy Policy, now time.Time) CircuitBreaker {
	breaker = normalizeBreaker(breaker)
	if breaker.State != BreakerOpen || breaker.OpenUntil == "" {
		return breaker
	}
	openUntil := parseOptionalTime(breaker.OpenUntil)
	if openUntil == nil || now.Before(*openUntil) {
		return breaker
	}
	before := breaker
	breaker.State = BreakerHalfOpen
	breaker.StateReason = ReasonCooldownElapsed
	breaker.HalfOpenProbeBudget = policy.HalfOpenProbeBudget
	breaker.HalfOpenProbeCount = 0
	breaker.SuccessCount = 0
	breaker.ProbeLeaseOwner = ""
	breaker.ProbeLeaseExpiresAt = ""
	if breakerChanged(before, breaker) {
		breaker.RecordVersion = before.RecordVersion + 1
	}
	return breaker
}

func observationCooldownUntil(observation Observation, breaker CircuitBreaker, policy Policy, observedAt time.Time) time.Time {
	if retry := ParseRetryAfter(observation.RetryAfter, observedAt); retry != "" {
		return parseTimeOr(retry, observedAt)
	}
	if observation.CooldownUntil != "" {
		return parseTimeOr(observation.CooldownUntil, observedAt)
	}
	cooldown := policy.BaseCooldown
	for i := 1; i < breaker.FailureCount; i++ {
		cooldown *= 2
		if cooldown >= policy.MaxCooldown {
			cooldown = policy.MaxCooldown
			break
		}
	}
	if policy.Jitter > 0 {
		cooldown += deterministicJitter(policy, breaker, observation)
		if cooldown > policy.MaxCooldown {
			cooldown = policy.MaxCooldown
		}
	}
	return observedAt.Add(cooldown)
}

func deterministicJitter(policy Policy, breaker CircuitBreaker, observation Observation) time.Duration {
	if policy.Jitter <= 0 {
		return 0
	}
	mod := policy.Jitter
	const maxSafeJitter = time.Duration(1 << 56)
	if mod > maxSafeJitter {
		mod = maxSafeJitter
	}
	hex := hashHex(policy.RandomSeed, breaker.CircuitBreakerID, observation.AvailabilityObservationID, observation.ObservedAt)
	var n time.Duration
	for _, ch := range hex[:16] {
		var digit time.Duration
		switch {
		case ch >= '0' && ch <= '9':
			digit = time.Duration(ch - '0')
		case ch >= 'a' && ch <= 'f':
			digit = time.Duration(ch-'a') + 10
		}
		n = (n*16 + digit) % mod
	}
	return n
}

func breakerEventForObservation(observation Observation) (BreakerKind, breakerEvent, ReasonCode, bool) {
	reason := observation.FailureClass
	if reason == "" && len(observation.ReasonCodes) > 0 {
		reason = observation.ReasonCodes[0]
	}
	switch observation.ObservationKind {
	case ObservationRateLimited:
		return BreakerRateLimit, eventRateLimit, ReasonRateLimited429, true
	case ObservationQuotaExhausted:
		return BreakerQuota, eventQuotaExhausted, ReasonQuotaExhausted, true
	case ObservationAuthFailure:
		return BreakerAuth, eventAuthFailure, ReasonAuth, true
	case ObservationModelUnavailable:
		return BreakerModel, eventModelUnavailable, ReasonModelUnavailable, true
	case ObservationProviderOutage:
		return BreakerHealth, eventProviderOutage, ReasonProviderOutage, true
	case ObservationMalformedResponse:
		return BreakerHealth, eventMalformed, ReasonMalformedResponse, true
	case ObservationTransportFailure:
		return BreakerTransport, eventTransientFailure, firstReason(reason, ReasonTransport), true
	case ObservationProbeFailure, ObservationLaunchFailure:
		return BreakerTransport, eventProbeFailure, firstReason(reason, ReasonTransport), true
	default:
		return "", "", "", false
	}
}

func breakerScope(kind BreakerKind, scope Scope) Scope {
	scope = normalizeScope(scope)
	out := Scope{
		ProjectID:              scope.ProjectID,
		AdapterID:              scope.AdapterID,
		ProviderInstallationID: scope.ProviderInstallationID,
	}
	switch kind {
	case BreakerAuth:
		out.AccountProfileID = scope.AccountProfileID
	case BreakerModel, BreakerQuota, BreakerRateLimit, BreakerTransport:
		out.AccountProfileID = scope.AccountProfileID
		out.ModelCapabilityID = scope.ModelCapabilityID
		out.CanonicalModelID = scope.CanonicalModelID
	}
	return out
}

func isSuccessObservation(observation Observation) bool {
	return observation.ObservationKind == ObservationProbeSuccess || observation.ObservationKind == ObservationLaunchSuccess
}

func observationAlreadyApplied(breaker CircuitBreaker, observation Observation) bool {
	if breaker.LastObservationAt == "" {
		return breaker.LastObservationID == observation.AvailabilityObservationID
	}
	if observation.ObservedAt < breaker.LastObservationAt {
		return true
	}
	if observation.ObservedAt == breaker.LastObservationAt && observation.AvailabilityObservationID <= breaker.LastObservationID {
		return true
	}
	return false
}

func parseOptionalTime(value string) *time.Time {
	if value == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}

func parseTimeOr(value string, fallback time.Time) time.Time {
	if parsed := parseOptionalTime(value); parsed != nil {
		return *parsed
	}
	return fallback.UTC()
}

func firstReason(value ReasonCode, fallback ReasonCode) ReasonCode {
	if value != "" {
		return value
	}
	return fallback
}

func breakerChanged(a, b CircuitBreaker) bool {
	a.RecordVersion = 0
	b.RecordVersion = 0
	return mustJSON(a) != mustJSON(b)
}
