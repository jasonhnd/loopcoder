package availability

import (
	"sort"
	"time"

	"github.com/jasonhnd/loopcoder/internal/budget"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

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
