package availability

import (
	"sort"
	"time"

	"github.com/jasonhnd/loopcoder/internal/budget"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

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
		if breaker.State == BreakerOpen || breaker.State == BreakerHalfOpen {
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
		if breaker.BreakerKind == BreakerRateLimit && (breaker.State == BreakerOpen || breaker.State == BreakerHalfOpen) {
			component.Score = 0
			component.Hard = true
			component.ReasonCodes = []ReasonCode{ReasonRateLimited429, ReasonOpenBreaker}
			component.EvidenceRecordIDs = append(component.EvidenceRecordIDs, breaker.CircuitBreakerID)
			component.Explanation = "rate-limit breaker blocks normal launch"
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
