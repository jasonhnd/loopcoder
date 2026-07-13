package routing

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/availability"
	"github.com/jasonhnd/loopcoder/internal/budget"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/runtimecap"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

func TestHardEligibilityRejectsUnsuitableHighQuotaCandidateBeforeScoring(t *testing.T) {
	fixture := newFixture(t)
	req := workerRequirement("task-route")
	candidate := fixture.candidate("codex", "acct-a", "codex-broken")
	result := FilterHardEligibility(Inputs{
		Requirement:  req,
		Candidates:   []Candidate{candidate},
		Inventory:    fixture.inventory,
		Availability: fixture.availabilityScores(),
		Policy: Policy{
			RequireExactQuota:           true,
			RequireAvailabilityEvidence: true,
		},
		RuntimeContract: fixture.contract,
		HostName:        "codex-cli",
	})

	if len(result.Eligible) != 0 {
		t.Fatalf("eligible = %#v, want unsuitable high-quota candidate rejected", result.Eligible)
	}
	if !rejectedHas(result, candidate.RoutingCandidateID, RejectModelUnavailable) {
		t.Fatalf("rejections = %#v, want model-unavailable", result.Rejected)
	}
	if !rejectedHas(result, candidate.RoutingCandidateID, RejectAvailabilityHardIneligible) {
		t.Fatalf("rejections = %#v, want availability-hard-ineligible", result.Rejected)
	}
}

func TestPinnedCandidateCannotBypassHardSafetyOrCapabilityConstraints(t *testing.T) {
	fixture := newFixture(t)
	req := workerRequirement("task-pin")
	pinned := fixture.candidate("codex", "acct-a", "codex-broken")
	other := fixture.candidate("claude", "acct-c", "claude-good")
	result := FilterHardEligibility(Inputs{
		Requirement:     req,
		Candidates:      []Candidate{other, pinned},
		Inventory:       fixture.inventory,
		Availability:    fixture.availabilityScores(),
		RuntimeContract: fixture.contract,
		HostName:        "codex-cli",
		Policy: Policy{
			RequireExactQuota:           true,
			RequireAvailabilityEvidence: true,
		},
		Pins: []Pin{{PinID: "pin_broken_model", AdapterID: "codex", ModelCapabilityID: "codex-broken"}},
	})

	if len(result.Eligible) != 0 {
		t.Fatalf("eligible = %#v, want pin to narrow set without making broken candidate eligible", result.Eligible)
	}
	for _, want := range []RejectionCode{RejectPinnedCandidateIneligible, RejectModelUnavailable} {
		if !rejectedHas(result, pinned.RoutingCandidateID, want) {
			t.Fatalf("pinned candidate rejections = %#v, want %s", result.Rejected, want)
		}
	}
	if !rejectedHas(result, other.RoutingCandidateID, RejectPinnedCandidateNotMatched) {
		t.Fatalf("unpinned candidate rejections = %#v, want pinned-candidate-not-matched", result.Rejected)
	}
}

func TestUnknownAndStaleEvidencePolicyIsVisibleAndControlled(t *testing.T) {
	fixture := newFixture(t)
	req := workerRequirement("task-stale")
	stale := fixture.candidate("codex", "acct-stale", "codex-good")

	rejected := FilterHardEligibility(Inputs{
		Requirement:     req,
		Candidates:      []Candidate{stale},
		Inventory:       fixture.inventory,
		RuntimeContract: fixture.contract,
		HostName:        "codex-cli",
		Policy:          Policy{RequireExactQuota: true},
	})
	if len(rejected.Eligible) != 0 {
		t.Fatalf("eligible stale candidate = %#v, want rejected", rejected.Eligible)
	}
	if !rejectedHas(rejected, stale.RoutingCandidateID, RejectEvidenceStale) {
		t.Fatalf("rejections = %#v, want evidence-stale", rejected.Rejected)
	}
	if !rejectedHas(rejected, stale.RoutingCandidateID, RejectQuotaConfidenceInsufficient) {
		t.Fatalf("rejections = %#v, want quota-confidence-insufficient", rejected.Rejected)
	}

	estimated := fixture.candidate("claude", "acct-c", "claude-good")
	allowed := FilterHardEligibility(Inputs{
		Requirement:     req,
		Candidates:      []Candidate{estimated},
		Inventory:       fixture.inventory,
		Budgets:         fixture.budgets,
		RuntimeContract: fixture.contract,
		HostName:        "codex-cli",
		Policy:          Policy{EvidencePolicy: EvidenceAllowEstimated},
	})
	if len(allowed.Eligible) != 1 {
		t.Fatalf("eligible estimated candidate = %#v rejected=%#v, want policy-controlled pass", allowed.Eligible, allowed.Rejected)
	}
}

func TestHardEligibilityIsDeterministicForIdenticalInputs(t *testing.T) {
	fixture := newFixture(t)
	req := workerRequirement("task-deterministic")
	inputs := Inputs{
		Requirement:     req,
		Inventory:       fixture.inventory,
		Availability:    fixture.availabilityScores(),
		RuntimeContract: fixture.contract,
		HostName:        "codex-cli",
		Policy: Policy{
			RequireExactQuota:           true,
			RequireAvailabilityEvidence: true,
			RequireBudgetEvidence:       true,
		},
	}
	first := FilterHardEligibility(inputs)
	second := FilterHardEligibility(inputs)
	if !reflect.DeepEqual(first, second) {
		left, _ := json.MarshalIndent(first, "", "  ")
		right, _ := json.MarshalIndent(second, "", "  ")
		t.Fatalf("filter is not deterministic:\n%s\n---\n%s", left, right)
	}
}

func TestFixturesCoverMultipleAccountsModelsHostsAndVerifierConstraints(t *testing.T) {
	fixture := newFixture(t)
	req := verifierRequirement("task-verify")
	worker := fixture.candidate("codex", "acct-a", "codex-good")
	candidates := []Candidate{
		fixture.candidate("codex", "acct-a", "codex-good"),
		fixture.candidate("codex", "acct-b", "codex-verifier"),
		fixture.candidate("claude", "acct-c", "claude-good"),
		fixture.candidate("paseo", "acct-p", "paseo-good"),
	}
	for i := range candidates {
		candidates[i].RoleKey = "verifier"
		candidates[i].Permission = taskrequirements.PermissionReadOnly
	}
	result := FilterHardEligibility(Inputs{
		Requirement:     req,
		Candidates:      candidates,
		Inventory:       fixture.inventory,
		Budgets:         fixture.budgets,
		RuntimeContract: fixture.contract,
		HostName:        "codex-cli",
		Policy: Policy{
			EvidencePolicy:       EvidenceAllowEstimated,
			VerifierIndependence: taskrequirements.IndependenceDifferentAccount,
		},
		WorkerRoute: &worker,
	})

	if len(result.Eligible) != 2 {
		t.Fatalf("eligible verifier candidates = %#v rejected=%#v, want codex acct-b and claude", result.Eligible, result.Rejected)
	}
	if !containsCandidate(result.Eligible, candidates[1].RoutingCandidateID) || !containsCandidate(result.Eligible, candidates[2].RoutingCandidateID) {
		t.Fatalf("eligible = %#v, want different account and different provider candidates", result.Eligible)
	}
	if !rejectedHas(result, candidates[0].RoutingCandidateID, RejectVerifierIndependenceInsufficient) {
		t.Fatalf("same-account verifier rejections = %#v, want independence rejection", result.Rejected)
	}
	if !rejectedHas(result, candidates[3].RoutingCandidateID, RejectJSONOutputUnsupported) {
		t.Fatalf("paseo verifier rejections = %#v, want host/provider JSON incompatibility", result.Rejected)
	}

	limitedHost := FilterHardEligibility(Inputs{
		Requirement:     req,
		Candidates:      []Candidate{candidates[1]},
		Inventory:       fixture.inventory,
		Budgets:         fixture.budgets,
		RuntimeContract: fixture.contract,
		HostName:        "limited-host",
		Policy:          Policy{EvidencePolicy: EvidenceAllowEstimated},
		WorkerRoute:     &worker,
	})
	if !rejectedHas(limitedHost, candidates[1].RoutingCandidateID, RejectRoleUnsupported) {
		t.Fatalf("limited host rejections = %#v, want host compatibility rejection", limitedHost.Rejected)
	}
}

type hardFixture struct {
	now       time.Time
	inventory providerinventory.Report
	contract  runtimecap.Contract
	budgets   []budget.Summary
}

func newFixture(t *testing.T) hardFixture {
	t.Helper()
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	projectID := "proj-routing"
	installations := []providerinventory.ProviderInstallation{
		installation(projectID, "pinst-codex", "codex", now, providerinventory.FreshnessFresh, providerinventory.ConfidenceExact),
		installation(projectID, "pinst-claude", "claude", now, providerinventory.FreshnessFresh, providerinventory.ConfidenceExact),
		installation(projectID, "pinst-paseo", "paseo", now, providerinventory.FreshnessFresh, providerinventory.ConfidenceExact),
	}
	accounts := []providerinventory.AccountProfile{
		account(projectID, "acct-a", "codex", "pinst-codex", now, providerinventory.FreshnessFresh, providerinventory.ConfidenceExact),
		account(projectID, "acct-b", "codex", "pinst-codex", now, providerinventory.FreshnessFresh, providerinventory.ConfidenceExact),
		account(projectID, "acct-stale", "codex", "pinst-codex", now, providerinventory.FreshnessStale, providerinventory.ConfidenceStale),
		account(projectID, "acct-c", "claude", "pinst-claude", now, providerinventory.FreshnessFresh, providerinventory.ConfidenceExact),
		account(projectID, "acct-p", "paseo", "pinst-paseo", now, providerinventory.FreshnessFresh, providerinventory.ConfidenceExact),
	}
	auths := []providerinventory.AuthReadiness{
		auth(projectID, "auth-acct-a", "codex", "pinst-codex", "acct-a", now, providerinventory.FreshnessFresh, providerinventory.ConfidenceExact),
		auth(projectID, "auth-acct-b", "codex", "pinst-codex", "acct-b", now, providerinventory.FreshnessFresh, providerinventory.ConfidenceExact),
		auth(projectID, "auth-acct-stale", "codex", "pinst-codex", "acct-stale", now, providerinventory.FreshnessStale, providerinventory.ConfidenceStale),
		auth(projectID, "auth-acct-c", "claude", "pinst-claude", "acct-c", now, providerinventory.FreshnessFresh, providerinventory.ConfidenceExact),
		auth(projectID, "auth-acct-p", "paseo", "pinst-paseo", "acct-p", now, providerinventory.FreshnessFresh, providerinventory.ConfidenceExact),
	}
	models := []providerinventory.ModelCapability{
		model("codex", "codex-good", "gpt-5.5", []providerinventory.CatalogRole{providerinventory.CatalogRoleWorker}, now, goodCaps()),
		model("codex", "codex-verifier", "gpt-5.5-verify", []providerinventory.CatalogRole{providerinventory.CatalogRoleVerifier}, now, verifierCaps()),
		model("codex", "codex-broken", "gpt-broken", []providerinventory.CatalogRole{providerinventory.CatalogRoleWorker}, now, goodCaps()),
		model("claude", "claude-good", "claude-opus", []providerinventory.CatalogRole{providerinventory.CatalogRoleWorker, providerinventory.CatalogRoleVerifier}, now, verifierCaps()),
		model("paseo", "paseo-good", "paseo-model", []providerinventory.CatalogRole{providerinventory.CatalogRoleVerifier}, now, verifierCaps()),
	}
	models[2].AvailabilityState = providerinventory.AvailabilityRemoved
	models[2].LifecycleState = providerinventory.LifecycleRemoved
	quotas := []providerinventory.QuotaSnapshot{
		quota("qsnap-codex-a-good", "codex", "pinst-codex", "acct-a", "codex-good", providerinventory.ConfidenceExact, providerinventory.FreshnessFresh, 500, now),
		quota("qsnap-codex-b-verifier", "codex", "pinst-codex", "acct-b", "codex-verifier", providerinventory.ConfidenceExact, providerinventory.FreshnessFresh, 400, now),
		quota("qsnap-codex-broken-high", "codex", "pinst-codex", "acct-a", "codex-broken", providerinventory.ConfidenceExact, providerinventory.FreshnessFresh, 999999999, now),
		quota("qsnap-codex-stale", "codex", "pinst-codex", "acct-stale", "codex-good", providerinventory.ConfidenceStale, providerinventory.FreshnessStale, 100, now),
		quota("qsnap-claude-estimated", "claude", "pinst-claude", "acct-c", "claude-good", providerinventory.ConfidenceEstimated, providerinventory.FreshnessFresh, 250, now),
		quota("qsnap-paseo-estimated", "paseo", "pinst-paseo", "acct-p", "paseo-good", providerinventory.ConfidenceEstimated, providerinventory.FreshnessFresh, 250, now),
	}
	budgets := []budget.Summary{
		budgetSummary("bpol-codex-a", "codex", "acct-a", "codex-good", 100),
		budgetSummary("bpol-codex-b", "codex", "acct-b", "codex-verifier", 100),
		budgetSummary("bpol-claude", "claude", "acct-c", "claude-good", 100),
		budgetSummary("bpol-paseo", "paseo", "acct-p", "paseo-good", 100),
	}
	return hardFixture{
		now: now,
		inventory: providerinventory.Report{
			SchemaVersion:     providerinventory.ProviderInventoryJSONSchema,
			GeneratedAt:       now.Format(time.RFC3339Nano),
			Confidence:        providerinventory.ConfidenceExact,
			Installations:     installations,
			AccountProfiles:   accounts,
			AuthReadiness:     auths,
			ModelCapabilities: models,
			QuotaSnapshots:    quotas,
		},
		contract: runtimecap.Contract{
			Providers: []runtimecap.ProviderRuntime{
				providerRuntime("codex", true, true, true),
				providerRuntime("claude", true, true, true),
				providerRuntime("paseo", true, false, true),
			},
			Hosts: []runtimecap.HostRuntime{
				{Name: "codex-cli", InvocationStyle: "cli", PreservesStdout: true, PreservesStderr: true, SupportsJSONOutput: true, SupportsTimeouts: true, SupportsCancel: true},
				{Name: "limited-host", InvocationStyle: "cli", PreservesStdout: true, PreservesStderr: true, SupportsJSONOutput: false, SupportsTimeouts: true, SupportsCancel: true},
			},
		},
		budgets: budgets,
	}
}

func (f hardFixture) candidate(adapterID, accountID, modelID string) Candidate {
	installationID := "pinst-" + adapterID
	authID := "auth-" + accountID
	c := Candidate{
		TaskID:                 "task",
		RoleKey:                "worker",
		AdapterID:              adapterID,
		ProviderInstallationID: installationID,
		AccountProfileID:       accountID,
		AuthReadinessID:        authID,
		ModelCapabilityID:      modelID,
		InvocationProfileKey:   "default",
		Permission:             taskrequirements.PermissionWrite,
		LaunchSideEffectClass:  taskrequirements.SideEffectProviderLaunch,
		NetworkPermission:      providerinventory.NetworkNotNeeded,
		ScopeBounded:           true,
		QualityFloor:           taskrequirements.QualityStandard,
		QuotaSnapshotIDs:       quotaIDsFor(adapterID, accountID, modelID, f.inventory.QuotaSnapshots),
		CapabilitySummary:      map[string]any{},
	}
	for _, b := range f.budgets {
		if b.Scope.AdapterID == adapterID && b.Scope.AccountProfileID == accountID && b.Scope.ModelCapabilityID == modelID {
			c.BudgetPolicyIDs = []string{b.BudgetPolicyID}
		}
	}
	c.RoutingCandidateID = candidateID(c)
	c.CandidateFingerprint = candidateFingerprint(c)
	return c
}

func (f hardFixture) availabilityScores() []availability.Score {
	var scores []availability.Score
	for _, q := range f.inventory.QuotaSnapshots {
		scope := availability.Scope{
			AdapterID:              q.AdapterID,
			ProviderInstallationID: ptr(q.ProviderInstallationID),
			AccountProfileID:       ptr(q.AccountProfileID),
			ModelCapabilityID:      ptr(q.ModelCapabilityID),
		}
		hard := []availability.ReasonCode{}
		if ptr(q.ModelCapabilityID) == "codex-broken" {
			hard = []availability.ReasonCode{availability.ReasonModelUnavailable}
		}
		score := availability.Score{
			SchemaVersion:         availability.ScoreSchema,
			AvailabilityScoreID:   "avscore-" + ptr(q.ModelCapabilityID) + "-" + ptr(q.AccountProfileID),
			Scope:                 scope,
			Eligible:              len(hard) == 0,
			HardIneligibleReasons: hard,
			EvidenceRecordIDs:     []string{q.QuotaSnapshotID},
			QuotaSnapshotIDs:      []string{q.QuotaSnapshotID},
		}
		scores = append(scores, score)
	}
	return scores
}

func workerRequirement(taskID string) taskrequirements.TaskRequirement {
	return taskrequirements.TaskRequirement{
		TaskID:             taskID,
		RoleKey:            "worker",
		RiskTier:           taskrequirements.RiskMedium,
		PermissionRequired: taskrequirements.PermissionWrite,
		SideEffectClass:    taskrequirements.SideEffectProviderLaunch,
		RequiredCapabilities: []taskrequirements.CapabilityRequirement{{
			Dimension:         taskrequirements.CapabilityCancellation,
			RequiredValue:     true,
			MinimumConfidence: providerinventory.ConfidenceExact,
			FreshnessRequired: providerinventory.FreshnessFresh,
		}},
		NetworkRequired:      taskrequirements.NetworkNotNeeded,
		CancellationRequired: true,
		QualityFloor:         taskrequirements.QualityStandard,
	}
}

func verifierRequirement(taskID string) taskrequirements.TaskRequirement {
	req := workerRequirement(taskID)
	req.RoleKey = "verifier"
	req.RiskTier = taskrequirements.RiskHigh
	req.PermissionRequired = taskrequirements.PermissionReadOnly
	req.RequiredOutput = taskrequirements.OutputVerificationVerdict
	req.RequiredCapabilities = []taskrequirements.CapabilityRequirement{
		{Dimension: taskrequirements.CapabilityReadOnly, RequiredValue: true, MinimumConfidence: providerinventory.ConfidenceExact, FreshnessRequired: providerinventory.FreshnessFresh},
		{Dimension: taskrequirements.CapabilityJSONOutput, RequiredValue: true, MinimumConfidence: providerinventory.ConfidenceExact, FreshnessRequired: providerinventory.FreshnessFresh},
		{Dimension: taskrequirements.CapabilityCancellation, RequiredValue: true, MinimumConfidence: providerinventory.ConfidenceExact, FreshnessRequired: providerinventory.FreshnessFresh},
	}
	req.VerificationRequirements = []taskrequirements.VerificationRequirement{{
		VerificationKind:     taskrequirements.VerificationLoopReview,
		RequiredForRiskTiers: []taskrequirements.RiskTier{taskrequirements.RiskHigh},
		IndependenceLevel:    taskrequirements.IndependenceDifferentAccount,
		PermissionRequired:   taskrequirements.PermissionReadOnly,
		OutputContract:       taskrequirements.OutputVerificationVerdict,
	}}
	return req
}

type caps struct {
	readOnly     providerinventory.CapabilityState
	jsonOutput   providerinventory.CapabilityState
	nested       providerinventory.CapabilityState
	mcp          providerinventory.CapabilityState
	cancellation providerinventory.CapabilityState
	usage        providerinventory.CapabilityState
}

func goodCaps() caps {
	return caps{cancellation: providerinventory.CapabilityTrue}
}

func verifierCaps() caps {
	return caps{readOnly: providerinventory.CapabilityTrue, jsonOutput: providerinventory.CapabilityTrue, cancellation: providerinventory.CapabilityTrue}
}

func model(adapterID, modelID, canonicalID string, roles []providerinventory.CatalogRole, now time.Time, c caps) providerinventory.ModelCapability {
	return providerinventory.ModelCapability{
		SchemaVersion:          providerinventory.ModelCapabilitySchema,
		RecordVersion:          1,
		ModelCapabilityID:      modelID,
		ModelCatalogSnapshotID: "mcats-" + adapterID,
		AdapterID:              adapterID,
		CanonicalModelID:       canonicalID,
		LifecycleState:         providerinventory.LifecycleAvailable,
		AvailabilityState:      providerinventory.AvailabilityAvailable,
		RolesSupported:         roles,
		ReadOnly:               c.readOnly,
		JSONOutput:             c.jsonOutput,
		NestedSubagents:        c.nested,
		MCPConfig:              c.mcp,
		Cancellation:           c.cancellation,
		TokenUsageReporting:    c.usage,
		Aliases:                []providerinventory.ModelAlias{},
		EntrySources:           []providerinventory.CatalogEntrySource{},
		CapturedAt:             now.Format(time.RFC3339Nano),
		StaleAfter:             now.Add(time.Hour).Format(time.RFC3339Nano),
		FreshnessState:         providerinventory.FreshnessFresh,
		Confidence:             providerinventory.ConfidenceExact,
	}
}

func installation(projectID, id, adapterID string, now time.Time, freshness providerinventory.FreshnessState, confidence providerinventory.Confidence) providerinventory.ProviderInstallation {
	return providerinventory.ProviderInstallation{
		SchemaVersion:          providerinventory.ProviderInstallationSchema,
		RecordVersion:          1,
		ProjectID:              &projectID,
		ProviderInstallationID: id,
		AdapterID:              adapterID,
		InstallationState:      providerinventory.InstallationInstalled,
		UsableForInvocation:    "yes",
		CapturedAt:             now.Format(time.RFC3339Nano),
		StaleAfter:             now.Add(time.Hour).Format(time.RFC3339Nano),
		FreshnessState:         freshness,
		Confidence:             confidence,
	}
}

func account(projectID, id, adapterID, installationID string, now time.Time, freshness providerinventory.FreshnessState, confidence providerinventory.Confidence) providerinventory.AccountProfile {
	return providerinventory.AccountProfile{
		SchemaVersion:          providerinventory.AccountProfileSchema,
		RecordVersion:          1,
		ProjectID:              &projectID,
		AccountProfileID:       id,
		AdapterID:              adapterID,
		ProviderInstallationID: &installationID,
		CapturedAt:             now.Format(time.RFC3339Nano),
		FreshnessState:         freshness,
		Confidence:             confidence,
	}
}

func auth(projectID, id, adapterID, installationID, accountID string, now time.Time, freshness providerinventory.FreshnessState, confidence providerinventory.Confidence) providerinventory.AuthReadiness {
	return providerinventory.AuthReadiness{
		SchemaVersion:          providerinventory.AuthReadinessSchema,
		RecordVersion:          1,
		ProjectID:              &projectID,
		AuthReadinessID:        id,
		AdapterID:              adapterID,
		ProviderInstallationID: &installationID,
		AccountProfileID:       &accountID,
		ReadinessState:         providerinventory.ReadinessReady,
		ReadinessConfidence:    confidence,
		CapturedAt:             now.Format(time.RFC3339Nano),
		FreshnessState:         freshness,
		Confidence:             confidence,
	}
}

func quota(id, adapterID, installationID, accountID, modelID string, confidence providerinventory.Confidence, freshness providerinventory.FreshnessState, remaining int64, now time.Time) providerinventory.QuotaSnapshot {
	return providerinventory.QuotaSnapshot{
		SchemaVersion:          providerinventory.QuotaSnapshotSchema,
		RecordVersion:          1,
		QuotaSnapshotID:        id,
		QuotaSourceID:          "qsrc-fixture",
		SourceKind:             providerinventory.QuotaSourceFixture,
		AdapterID:              adapterID,
		ProviderInstallationID: &installationID,
		AccountProfileID:       &accountID,
		ModelCapabilityID:      &modelID,
		QuantityKind:           providerinventory.QuantityRequests,
		Unit:                   "request",
		WindowKind:             providerinventory.WindowFixedHour,
		ResetSemantics:         providerinventory.ResetWindowBoundary,
		RemainingValue:         &remaining,
		ValueScale:             0,
		Confidence:             confidence,
		FreshnessState:         freshness,
		CapturedAt:             now.Format(time.RFC3339Nano),
		StaleAfter:             now.Add(time.Hour).Format(time.RFC3339Nano),
	}
}

func budgetSummary(id, adapterID, accountID, modelID string, available int64) budget.Summary {
	return budget.Summary{
		BudgetPolicyID: id,
		Scope: budget.Scope{
			AdapterID:         adapterID,
			AccountProfileID:  accountID,
			ModelCapabilityID: modelID,
		},
		PolicyMode:     budget.PolicyHard,
		AvailableValue: available,
		Confidence:     providerinventory.ConfidenceExact,
	}
}

func providerRuntime(name string, readOnly, jsonOutput, cancellation bool) runtimecap.ProviderRuntime {
	return runtimecap.ProviderRuntime{
		Name:                name,
		DisplayName:         name,
		Executable:          name,
		ReadOnly:            readOnly,
		JSONOutput:          jsonOutput,
		Cancellation:        cancellation,
		TokenUsageReporting: true,
	}
}

func quotaIDsFor(adapterID, accountID, modelID string, snapshots []providerinventory.QuotaSnapshot) []string {
	var ids []string
	for _, q := range snapshots {
		if q.AdapterID == adapterID && ptr(q.AccountProfileID) == accountID && ptr(q.ModelCapabilityID) == modelID {
			ids = append(ids, q.QuotaSnapshotID)
		}
	}
	return ids
}

func containsCandidate(candidates []Candidate, id string) bool {
	for _, candidate := range candidates {
		if candidate.RoutingCandidateID == id {
			return true
		}
	}
	return false
}

func rejectedHas(result Result, candidateID string, code RejectionCode) bool {
	for _, rejected := range result.Rejected {
		if rejected.Candidate.RoutingCandidateID != candidateID {
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

func ptr(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
