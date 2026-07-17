package routing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/availability"
	"github.com/jasonhnd/loopcoder/internal/budget"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

// isolateStoredRouteEvidence keeps route inputs within the authority named by
// the persisted TaskRequirement. An empty scope dimension is global; a
// populated project, delivery-run, or task dimension must match exactly.
func isolateStoredRouteEvidence(inputs Inputs, requirement taskrequirements.TaskRequirement) (Inputs, error) {
	report, err := routeInventoryForRequirement(inputs.Inventory, requirement)
	if err != nil {
		return inputs, err
	}
	inputs.Inventory = report
	inputs.Availability = routeAvailabilityForRequirement(inputs.Availability, requirement)
	inputs.CircuitBreakers = routeBreakersForRequirement(inputs.CircuitBreakers, requirement)
	inputs.Budgets = routeBudgetsForRequirement(inputs.Budgets, requirement)
	role, hasRole := ResolveRoleDefinition(requirement.RoleKey, inputs.RoleDefinitions)
	inputs.Candidates = candidatesFromInventory(requirement, report, role, hasRole)
	return inputs, nil
}

func buildAndPersistStoredRoute(ctx context.Context, store storage.Store, input DecisionInput) (RoutingDecision, error) {
	decision, err := BuildRoutingDecision(input)
	if err != nil {
		return RoutingDecision{}, err
	}
	if err := PersistRoutingDecision(ctx, store, decision); err != nil {
		return RoutingDecision{}, err
	}
	stored, err := LoadRoutingDecision(ctx, store, decision.RoutingDecisionID)
	if err != nil {
		return RoutingDecision{}, err
	}
	if stored.TerminalErrorCode != "" {
		return stored, routingTerminalError(stored.TerminalErrorCode)
	}
	return stored, nil
}

func routeAvailabilityForRequirement(values []availability.Score, requirement taskrequirements.TaskRequirement) []availability.Score {
	out := make([]availability.Score, 0, len(values))
	for _, value := range values {
		if routeScopeMatches(value.Scope.ProjectID, value.Scope.DeliveryRunID, value.Scope.TaskID, requirement) {
			out = append(out, value)
		}
	}
	return out
}

func routeBreakersForRequirement(values []availability.CircuitBreaker, requirement taskrequirements.TaskRequirement) []availability.CircuitBreaker {
	out := make([]availability.CircuitBreaker, 0, len(values))
	for _, value := range values {
		if routeScopeMatches(value.Scope.ProjectID, value.Scope.DeliveryRunID, value.Scope.TaskID, requirement) {
			out = append(out, value)
		}
	}
	return out
}

func routeBudgetsForRequirement(values []budget.Summary, requirement taskrequirements.TaskRequirement) []budget.Summary {
	out := make([]budget.Summary, 0, len(values))
	for _, value := range values {
		if routeScopeMatches(value.Scope.ProjectID, value.Scope.DeliveryRunID, value.Scope.TaskID, requirement) {
			out = append(out, value)
		}
	}
	return out
}

func routeScopeMatches(projectID, deliveryRunID, taskID string, requirement taskrequirements.TaskRequirement) bool {
	return routeScopeDimensionMatches(projectID, requirement.ProjectID) &&
		routeScopeDimensionMatches(deliveryRunID, requirement.DeliveryRunID) &&
		routeScopeDimensionMatches(taskID, requirement.TaskID)
}

func routeScopeDimensionMatches(scopeValue, authorityValue string) bool {
	scopeValue = strings.TrimSpace(scopeValue)
	return scopeValue == "" || scopeValue == strings.TrimSpace(authorityValue)
}

// routeInventoryForRequirement filters directly project-scoped inventory and
// then follows its references. Records without a project scope remain global.
// A global child that points at a foreign scoped parent is excluded so a
// contradictory reference cannot re-introduce foreign authority.
func routeInventoryForRequirement(report providerinventory.Report, requirement taskrequirements.TaskRequirement) (providerinventory.Report, error) {
	out := report
	out.Installations = nil
	out.ProbeResults = nil
	out.AccountProfiles = nil
	out.AuthReadiness = nil
	out.ModelCatalogSnapshots = nil
	out.ModelCapabilities = nil
	out.QuotaSnapshots = nil

	knownInstallations := make(stringSet, len(report.Installations))
	allowedInstallations := make(stringSet, len(report.Installations))
	for _, value := range report.Installations {
		knownInstallations.add(value.ProviderInstallationID)
		if routeInventoryProjectMatches(value.ProjectID, requirement.ProjectID) {
			out.Installations = append(out.Installations, value)
			allowedInstallations.add(value.ProviderInstallationID)
		}
	}

	for _, value := range report.ProbeResults {
		if routeInventoryProjectMatches(value.ProjectID, requirement.ProjectID) &&
			routeInventoryReferenceMatches(value.ProviderInstallationID, knownInstallations, allowedInstallations) {
			out.ProbeResults = append(out.ProbeResults, value)
		}
	}

	knownAccounts := make(stringSet, len(report.AccountProfiles))
	allowedAccounts := make(stringSet, len(report.AccountProfiles))
	for _, value := range report.AccountProfiles {
		knownAccounts.add(value.AccountProfileID)
		if routeInventoryProjectMatches(value.ProjectID, requirement.ProjectID) &&
			routeInventoryReferenceMatches(value.ProviderInstallationID, knownInstallations, allowedInstallations) {
			out.AccountProfiles = append(out.AccountProfiles, value)
			allowedAccounts.add(value.AccountProfileID)
		}
	}

	knownReadiness := make(stringSet, len(report.AuthReadiness))
	allowedReadiness := make(stringSet, len(report.AuthReadiness))
	for _, value := range report.AuthReadiness {
		knownReadiness.add(value.AuthReadinessID)
		if routeInventoryProjectMatches(value.ProjectID, requirement.ProjectID) &&
			routeInventoryReferenceMatches(value.ProviderInstallationID, knownInstallations, allowedInstallations) &&
			routeInventoryReferenceMatches(value.AccountProfileID, knownAccounts, allowedAccounts) {
			out.AuthReadiness = append(out.AuthReadiness, value)
			allowedReadiness.add(value.AuthReadinessID)
		}
	}

	knownCatalogs := make(stringSet, len(report.ModelCatalogSnapshots))
	allowedCatalogs := make(stringSet, len(report.ModelCatalogSnapshots))
	for _, value := range report.ModelCatalogSnapshots {
		knownCatalogs.add(value.ModelCatalogSnapshotID)
		if routeInventoryReferenceMatches(value.ProviderInstallationID, knownInstallations, allowedInstallations) &&
			routeInventoryReferenceMatches(value.AccountProfileID, knownAccounts, allowedAccounts) &&
			routeInventoryReferenceMatches(value.AuthReadinessID, knownReadiness, allowedReadiness) {
			out.ModelCatalogSnapshots = append(out.ModelCatalogSnapshots, value)
			allowedCatalogs.add(value.ModelCatalogSnapshotID)
		}
	}

	knownModels := make(stringSet, len(report.ModelCapabilities))
	allowedModels := make(stringSet, len(report.ModelCapabilities))
	for _, value := range report.ModelCapabilities {
		knownModels.add(value.ModelCapabilityID)
		if routeInventoryStringReferenceMatches(value.ModelCatalogSnapshotID, knownCatalogs, allowedCatalogs) {
			out.ModelCapabilities = append(out.ModelCapabilities, value)
			allowedModels.add(value.ModelCapabilityID)
		}
	}

	for _, value := range report.QuotaSnapshots {
		if routeInventoryReferenceMatches(value.ProviderInstallationID, knownInstallations, allowedInstallations) &&
			routeInventoryReferenceMatches(value.AccountProfileID, knownAccounts, allowedAccounts) &&
			routeInventoryReferenceMatches(value.ModelCapabilityID, knownModels, allowedModels) {
			out.QuotaSnapshots = append(out.QuotaSnapshots, value)
		}
	}

	out.Confidence = routeInventoryConfidence(out.Installations, out.ProbeResults)
	fingerprint, err := routeInventoryFingerprint(out)
	if err != nil {
		return providerinventory.Report{}, err
	}
	out.InventoryFingerprint = fingerprint
	return out, nil
}

type stringSet map[string]struct{}

func (s stringSet) add(value string) {
	value = strings.TrimSpace(value)
	if value != "" {
		s[value] = struct{}{}
	}
}

func (s stringSet) has(value string) bool {
	_, ok := s[strings.TrimSpace(value)]
	return ok
}

func routeInventoryProjectMatches(projectID *string, authorityProjectID string) bool {
	if projectID == nil {
		return true
	}
	return routeScopeDimensionMatches(*projectID, authorityProjectID)
}

func routeInventoryReferenceMatches(reference *string, known, allowed stringSet) bool {
	if reference == nil {
		return true
	}
	return routeInventoryStringReferenceMatches(*reference, known, allowed)
}

func routeInventoryStringReferenceMatches(reference string, known, allowed stringSet) bool {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return true
	}
	// Some legacy cache fixtures contain no payload-level parent identities at
	// all. In that case there is no scoped parent authority to contradict. Once
	// the parent set is populated, however, an unknown or foreign reference is
	// excluded rather than treated as global.
	if len(known) == 0 {
		return true
	}
	return allowed.has(reference)
}

func routeInventoryConfidence(installations []providerinventory.ProviderInstallation, probes []providerinventory.ProbeResult) providerinventory.Confidence {
	if len(installations) == 0 {
		return providerinventory.ConfidenceUnavailable
	}
	for _, probe := range probes {
		if probe.ProbeKind != "native-federation" && probe.Outcome == providerinventory.OutcomeProbeFailed {
			return providerinventory.ConfidenceUnknown
		}
	}
	return providerinventory.ConfidenceExact
}

func routeInventoryFingerprint(report providerinventory.Report) (string, error) {
	clone := report
	clone.GeneratedAt = ""
	clone.InventoryFingerprint = ""
	payload, err := json.Marshal(clone)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
