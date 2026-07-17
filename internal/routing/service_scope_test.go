package routing

import (
	"testing"

	"github.com/jasonhnd/loopcoder/internal/availability"
	"github.com/jasonhnd/loopcoder/internal/budget"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

func TestStoredRouteForeignScopedEvidenceCannotAlterCandidate(t *testing.T) {
	tests := []struct {
		name       string
		addForeign func(*Inputs, availability.Scope)
		rejection  RejectionCode
	}{
		{
			name: "other project availability exhaustion",
			addForeign: func(inputs *Inputs, scope availability.Scope) {
				scope.ProjectID = "project-foreign"
				inputs.Availability = append(inputs.Availability, routeScopeExhaustedScore(scope))
			},
			rejection: RejectAvailabilityHardIneligible,
		},
		{
			name: "other run open breaker",
			addForeign: func(inputs *Inputs, scope availability.Scope) {
				scope.DeliveryRunID = "run-foreign"
				inputs.CircuitBreakers = append(inputs.CircuitBreakers, routeScopeOpenBreaker(scope))
			},
			rejection: RejectBreakerOpen,
		},
		{
			name: "other task exhausted budget",
			addForeign: func(inputs *Inputs, scope availability.Scope) {
				inputs.Budgets = append(inputs.Budgets, routeScopeExhaustedBudget(scope, "task-foreign"))
			},
			rejection: RejectBudgetExhausted,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inputs, requirement, scope := routeScopedEvidenceFixture(t)
			test.addForeign(&inputs, scope)

			isolated, err := isolateStoredRouteEvidence(inputs, requirement)
			if err != nil {
				t.Fatalf("isolateStoredRouteEvidence: %v", err)
			}
			result := FilterHardEligibility(isolated)
			if !routeScopeCandidateEligible(result, scope) {
				t.Fatalf("foreign evidence altered candidate: eligible=%#v rejected=%#v", result.Eligible, result.Rejected)
			}
			if routeScopeCandidateRejectedWith(result, scope, test.rejection) {
				t.Fatalf("foreign evidence produced %s: %#v", test.rejection, result.Rejected)
			}
		})
	}
}

func TestStoredRouteMatchingScopedEvidenceStillApplies(t *testing.T) {
	tests := []struct {
		name      string
		add       func(*Inputs, availability.Scope, taskrequirements.TaskRequirement)
		rejection RejectionCode
	}{
		{
			name: "project availability exhaustion",
			add: func(inputs *Inputs, scope availability.Scope, requirement taskrequirements.TaskRequirement) {
				scope.ProjectID = requirement.ProjectID
				inputs.Availability = append(inputs.Availability, routeScopeExhaustedScore(scope))
			},
			rejection: RejectAvailabilityHardIneligible,
		},
		{
			name: "run open breaker",
			add: func(inputs *Inputs, scope availability.Scope, requirement taskrequirements.TaskRequirement) {
				scope.DeliveryRunID = requirement.DeliveryRunID
				inputs.CircuitBreakers = append(inputs.CircuitBreakers, routeScopeOpenBreaker(scope))
			},
			rejection: RejectBreakerOpen,
		},
		{
			name: "task exhausted budget",
			add: func(inputs *Inputs, scope availability.Scope, requirement taskrequirements.TaskRequirement) {
				inputs.Budgets = append(inputs.Budgets, routeScopeExhaustedBudget(scope, requirement.TaskID))
			},
			rejection: RejectBudgetExhausted,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inputs, requirement, scope := routeScopedEvidenceFixture(t)
			test.add(&inputs, scope, requirement)

			isolated, err := isolateStoredRouteEvidence(inputs, requirement)
			if err != nil {
				t.Fatalf("isolateStoredRouteEvidence: %v", err)
			}
			result := FilterHardEligibility(isolated)
			if !routeScopeCandidateRejectedWith(result, scope, test.rejection) {
				t.Fatalf("matching evidence did not produce %s: eligible=%#v rejected=%#v", test.rejection, result.Eligible, result.Rejected)
			}
		})
	}
}

func TestStoredRouteInventoryProjectScopeAndReferencesAreIsolated(t *testing.T) {
	requirement := taskrequirements.TaskRequirement{ProjectID: "project-current"}
	report := providerinventory.Report{
		SchemaVersion: providerinventory.ProviderInventoryJSONSchema,
		Installations: []providerinventory.ProviderInstallation{
			routeScopeInstallation("installation-global", ""),
			routeScopeInstallation("installation-current", "project-current"),
			routeScopeInstallation("installation-foreign", "project-foreign"),
		},
		AccountProfiles: []providerinventory.AccountProfile{
			routeScopeAccount("account-global", "installation-global", ""),
			routeScopeAccount("account-current", "installation-current", "project-current"),
			routeScopeAccount("account-foreign", "installation-foreign", "project-foreign"),
			// A nominally global child cannot bypass its foreign scoped parent.
			routeScopeAccount("account-contradictory", "installation-foreign", ""),
		},
		AuthReadiness: []providerinventory.AuthReadiness{
			routeScopeAuth("auth-global", "installation-global", "account-global", ""),
			routeScopeAuth("auth-current", "installation-current", "account-current", "project-current"),
			routeScopeAuth("auth-foreign", "installation-foreign", "account-foreign", "project-foreign"),
		},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{
			routeScopeCatalog("catalog-global", "installation-global", "account-global", "auth-global"),
			routeScopeCatalog("catalog-current", "installation-current", "account-current", "auth-current"),
			routeScopeCatalog("catalog-foreign", "installation-foreign", "account-foreign", "auth-foreign"),
		},
		ModelCapabilities: []providerinventory.ModelCapability{
			{ModelCapabilityID: "model-global", ModelCatalogSnapshotID: "catalog-global"},
			{ModelCapabilityID: "model-current", ModelCatalogSnapshotID: "catalog-current"},
			{ModelCapabilityID: "model-foreign", ModelCatalogSnapshotID: "catalog-foreign"},
		},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{
			routeScopeQuota("quota-global", "installation-global", "account-global", "model-global"),
			routeScopeQuota("quota-current", "installation-current", "account-current", "model-current"),
			routeScopeQuota("quota-foreign", "installation-foreign", "account-foreign", "model-foreign"),
		},
	}

	filtered, err := routeInventoryForRequirement(report, requirement)
	if err != nil {
		t.Fatalf("routeInventoryForRequirement: %v", err)
	}
	assertRouteScopeIDs(t, "installations", []string{"installation-global", "installation-current"}, routeScopeInstallationIDs(filtered.Installations))
	assertRouteScopeIDs(t, "accounts", []string{"account-global", "account-current"}, routeScopeAccountIDs(filtered.AccountProfiles))
	assertRouteScopeIDs(t, "auth", []string{"auth-global", "auth-current"}, routeScopeAuthIDs(filtered.AuthReadiness))
	assertRouteScopeIDs(t, "catalogs", []string{"catalog-global", "catalog-current"}, routeScopeCatalogIDs(filtered.ModelCatalogSnapshots))
	assertRouteScopeIDs(t, "models", []string{"model-global", "model-current"}, routeScopeModelIDs(filtered.ModelCapabilities))
	assertRouteScopeIDs(t, "quota", []string{"quota-global", "quota-current"}, routeScopeQuotaIDs(filtered.QuotaSnapshots))

	withoutForeign := report
	withoutForeign.Installations = report.Installations[:2]
	withoutForeign.AccountProfiles = report.AccountProfiles[:2]
	withoutForeign.AuthReadiness = report.AuthReadiness[:2]
	withoutForeign.ModelCatalogSnapshots = report.ModelCatalogSnapshots[:2]
	withoutForeign.ModelCapabilities = report.ModelCapabilities[:2]
	withoutForeign.QuotaSnapshots = report.QuotaSnapshots[:2]
	baseline, err := routeInventoryForRequirement(withoutForeign, requirement)
	if err != nil {
		t.Fatalf("baseline routeInventoryForRequirement: %v", err)
	}
	if filtered.InventoryFingerprint != baseline.InventoryFingerprint {
		t.Fatalf("foreign inventory changed scoped fingerprint: filtered=%s baseline=%s", filtered.InventoryFingerprint, baseline.InventoryFingerprint)
	}
}

func routeScopedEvidenceFixture(t *testing.T) (Inputs, taskrequirements.TaskRequirement, availability.Scope) {
	t.Helper()
	fixture := newFixture(t)
	requirement := workerRequirement("task-current")
	requirement.ProjectID = "proj-routing"
	requirement.DeliveryRunID = "run-current"
	scope := availability.Scope{
		AdapterID:              "codex",
		ProviderInstallationID: "pinst-codex",
		AccountProfileID:       "acct-a",
		ModelCapabilityID:      "codex-good",
	}
	return Inputs{
		Requirement:     requirement,
		RoleDefinitions: BuiltInRoleDefinitions(),
		Inventory:       fixture.inventory,
		Availability:    fixture.availabilityScores(),
		Budgets:         fixture.budgets,
		RuntimeContract: fixture.contract,
		HostName:        "codex-cli",
		Policy:          Policy{RequireExactQuota: true, RequireAvailabilityEvidence: true, RequireBudgetEvidence: true},
		Now:             fixture.now,
	}, requirement, scope
}

func routeScopeExhaustedScore(scope availability.Scope) availability.Score {
	return availability.Score{
		AvailabilityScoreID:   "availability-scoped-exhausted",
		Scope:                 scope,
		Eligible:              false,
		HardIneligibleReasons: []availability.ReasonCode{availability.ReasonQuotaExhausted},
		EvidenceRecordIDs:     []string{"scoped-exhaustion"},
	}
}

func routeScopeOpenBreaker(scope availability.Scope) availability.CircuitBreaker {
	return availability.CircuitBreaker{
		CircuitBreakerID: "breaker-scoped-open",
		BreakerKind:      availability.BreakerQuota,
		State:            availability.BreakerOpen,
		Scope:            scope,
		StateReason:      availability.ReasonQuotaExhausted,
	}
}

func routeScopeExhaustedBudget(scope availability.Scope, taskID string) budget.Summary {
	return budget.Summary{
		BudgetPolicyID: "budget-scoped-exhausted",
		Scope: budget.Scope{
			ScopeKind:         budget.ScopeTask,
			ProjectID:         scope.ProjectID,
			DeliveryRunID:     scope.DeliveryRunID,
			TaskID:            taskID,
			AdapterID:         scope.AdapterID,
			AccountProfileID:  scope.AccountProfileID,
			ModelCapabilityID: scope.ModelCapabilityID,
		},
		PolicyMode:     budget.PolicyHard,
		AvailableValue: 0,
		Confidence:     providerinventory.ConfidenceExact,
	}
}

func routeScopeCandidateEligible(result Result, scope availability.Scope) bool {
	for _, candidate := range result.Eligible {
		if routeScopeIsCandidate(candidate, scope) {
			return true
		}
	}
	return false
}

func routeScopeCandidateRejectedWith(result Result, scope availability.Scope, code RejectionCode) bool {
	for _, rejected := range result.Rejected {
		if !routeScopeIsCandidate(rejected.Candidate, scope) {
			continue
		}
		for _, reason := range rejected.Reasons {
			if reason.Code == code {
				return true
			}
		}
	}
	return false
}

func routeScopeIsCandidate(candidate Candidate, scope availability.Scope) bool {
	return candidate.AdapterID == scope.AdapterID &&
		candidate.ProviderInstallationID == scope.ProviderInstallationID &&
		candidate.AccountProfileID == scope.AccountProfileID &&
		candidate.ModelCapabilityID == scope.ModelCapabilityID
}

func routeScopeProject(projectID string) *string {
	if projectID == "" {
		return nil
	}
	return &projectID
}

func routeScopeInstallation(id, projectID string) providerinventory.ProviderInstallation {
	return providerinventory.ProviderInstallation{ProviderInstallationID: id, ProjectID: routeScopeProject(projectID)}
}

func routeScopeAccount(id, installationID, projectID string) providerinventory.AccountProfile {
	return providerinventory.AccountProfile{AccountProfileID: id, ProviderInstallationID: &installationID, ProjectID: routeScopeProject(projectID)}
}

func routeScopeAuth(id, installationID, accountID, projectID string) providerinventory.AuthReadiness {
	return providerinventory.AuthReadiness{AuthReadinessID: id, ProviderInstallationID: &installationID, AccountProfileID: &accountID, ProjectID: routeScopeProject(projectID)}
}

func routeScopeCatalog(id, installationID, accountID, authID string) providerinventory.ModelCatalogSnapshot {
	return providerinventory.ModelCatalogSnapshot{ModelCatalogSnapshotID: id, ProviderInstallationID: &installationID, AccountProfileID: &accountID, AuthReadinessID: &authID}
}

func routeScopeQuota(id, installationID, accountID, modelID string) providerinventory.QuotaSnapshot {
	return providerinventory.QuotaSnapshot{QuotaSnapshotID: id, ProviderInstallationID: &installationID, AccountProfileID: &accountID, ModelCapabilityID: &modelID}
}

func assertRouteScopeIDs(t *testing.T, name string, want, got []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %#v, want %#v", name, got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("%s = %#v, want %#v", name, got, want)
		}
	}
}

func routeScopeInstallationIDs(values []providerinventory.ProviderInstallation) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, value.ProviderInstallationID)
	}
	return out
}

func routeScopeAccountIDs(values []providerinventory.AccountProfile) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, value.AccountProfileID)
	}
	return out
}

func routeScopeAuthIDs(values []providerinventory.AuthReadiness) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, value.AuthReadinessID)
	}
	return out
}

func routeScopeCatalogIDs(values []providerinventory.ModelCatalogSnapshot) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, value.ModelCatalogSnapshotID)
	}
	return out
}

func routeScopeModelIDs(values []providerinventory.ModelCapability) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, value.ModelCapabilityID)
	}
	return out
}

func routeScopeQuotaIDs(values []providerinventory.QuotaSnapshot) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, value.QuotaSnapshotID)
	}
	return out
}
