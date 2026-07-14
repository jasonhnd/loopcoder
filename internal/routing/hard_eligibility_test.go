package routing

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/availability"
	"github.com/jasonhnd/loopcoder/internal/budget"
	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/runtimecap"
	"github.com/jasonhnd/loopcoder/internal/storage"
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

func TestInputsWithCachedInventoryLoadsDurableQuotaWithoutCollectors(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, err := storage.Open(ctx, storage.Options{Path: tempDB(t), Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()
	seedCachedRoutingInventoryPayloads(t, ctx, store, fixture)
	inputs, err := InputsWithCachedInventory(ctx, store, Inputs{
		Requirement:     workerRequirement("task-cache"),
		RuntimeContract: fixture.contract,
		HostName:        "codex-cli",
		Policy: Policy{
			RequireExactQuota: true,
		},
	})
	if err != nil {
		t.Fatalf("InputsWithCachedInventory: %v", err)
	}
	if len(inputs.Inventory.QuotaSnapshots) == 0 {
		t.Fatalf("cached inventory has no quota snapshots: %#v", inputs.Inventory)
	}
	result := FilterHardEligibility(inputs)
	if len(result.Eligible) == 0 {
		t.Fatalf("eligible candidates = %#v rejected=%#v, want cached quota to drive routing", result.Eligible, result.Rejected)
	}
	for _, candidate := range result.Eligible {
		if candidate.AdapterID == "codex" && len(candidate.QuotaSnapshotIDs) == 0 {
			t.Fatalf("codex candidate missing cached quota ids: %#v", candidate)
		}
	}
}

func seedCachedRoutingInventoryPayloads(t *testing.T, ctx context.Context, store storage.Store, fixture hardFixture) {
	t.Helper()
	at := fixture.now.Format(time.RFC3339Nano)
	mustJSON := func(value any) string {
		t.Helper()
		payload, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal inventory payload: %v", err)
		}
		return string(payload)
	}
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		for _, installation := range fixture.inventory.Installations {
			installation.Scope = "machine"
			installation.ProjectID = nil
			installation.AdapterDeclarationID = "adecl-" + installation.AdapterID
			installation.ProviderDisplayName = installation.AdapterID
			installation.ExecutableName = installation.AdapterID
			installation.ExecutableIdentity = providerinventory.ExecutableIdentity{
				Basename:          installation.AdapterID,
				Platform:          "test",
				PathHash:          "sha256:test",
				SymlinkResolution: "not-symlink",
				ExecutableMode:    "executable",
			}
			installation.CanonicalPathRedacted = installation.AdapterID
			installation.DiscoverySource = providerinventory.DiscoveryFixture
			installation.DiscoveryOrder = 1
			installation.Platform = "test"
			installation.VersionConfidence = providerinventory.ConfidenceExact
			if installation.InstallationState == "" {
				installation.InstallationState = providerinventory.InstallationInstalled
			}
			if installation.UsableForInvocation == "" {
				installation.UsableForInvocation = "yes"
			}
			installation.KnownLimitations = []string{}
			installation.CreatedAt = firstNonEmpty(installation.CreatedAt, at)
			installation.UpdatedAt = firstNonEmpty(installation.UpdatedAt, at)
			installation.CreatedBy = providerinventory.ActorProvenance{ActorKind: "policy-engine", ActorID: "test", DecisionAuthority: "deterministic-policy-engine", Source: "test"}
			installation.UpdatedBy = installation.CreatedBy
			installation.Host = providerinventory.HostProvenance{HostKind: "generic-local", HostID: "test", ProcessID: 1, LoopcoderVersion: "test", Platform: "test"}
			installation.PolicyVersion = providerinventory.PolicyVersion
			installation.SideEffectClass = "local-read"
			installation.Classification = "provider-output-untrusted"
			installation.Source = providerinventory.SourceDescriptor{Kind: "fixture", AdapterID: installation.AdapterID}
			installation.Evidence = providerinventory.EvidenceSummary{Kind: string(providerinventory.EvidenceFileExistence), CommandBounded: true, NoShell: true}
			installation.GapReasons = []string{}
			payload := mustJSON(installation)
			if _, err := tx.Exec(ctx, `INSERT INTO adapter_declarations(
				adapter_declaration_id, schema_version, record_version, adapter_id, adapter_version, display_name,
				executable_names_json, created_at, updated_at, payload_json)
				VALUES (?, 'loopcoder.adapter_declaration.v1', 1, ?, 'test', ?, '[]', ?, ?, '{}')
				ON CONFLICT(adapter_declaration_id) DO NOTHING`,
				"adecl-"+installation.AdapterID, installation.AdapterID, installation.AdapterID, at, at); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO provider_installations(
				provider_installation_id, schema_version, record_version, scope, project_id, adapter_id, adapter_declaration_id,
				provider_display_name, executable_name, executable_identity_json, canonical_path_redacted, discovery_source,
				discovery_order, platform, version_confidence, installation_state, usable_for_invocation, created_at,
				updated_at, captured_at, stale_after, freshness_state, confidence, side_effect_class, classification, payload_json)
				VALUES (?, ?, ?, 'machine', NULL, ?, ?, ?, ?, '{}', ?, 'test',
				1, 'test', ?, ?, ?, ?, ?, ?, ?, ?, ?, 'local-read', 'local-diagnostic', ?)
				ON CONFLICT(provider_installation_id) DO UPDATE SET payload_json = excluded.payload_json`,
				installation.ProviderInstallationID, installation.SchemaVersion, installation.RecordVersion, installation.AdapterID,
				"adecl-"+installation.AdapterID, installation.AdapterID, installation.AdapterID, installation.AdapterID,
				installation.Confidence, installation.InstallationState, installation.UsableForInvocation, at, at, installation.CapturedAt,
				installation.StaleAfter, installation.FreshnessState, installation.Confidence, payload); err != nil {
				return err
			}
		}
		for _, account := range fixture.inventory.AccountProfiles {
			account.Scope = "machine"
			account.ProjectID = nil
			if account.ProfileSource == "" {
				account.ProfileSource = providerinventory.ProfileSourceFixture
			}
			if account.SelectionState == "" {
				account.SelectionState = providerinventory.SelectionDefault
			}
			account.CreatedAt = firstNonEmpty(account.CreatedAt, at)
			account.UpdatedAt = firstNonEmpty(account.UpdatedAt, at)
			account.CreatedBy = providerinventory.ActorProvenance{ActorKind: "policy-engine", ActorID: "test", DecisionAuthority: "deterministic-policy-engine", Source: "test"}
			account.UpdatedBy = account.CreatedBy
			account.Host = providerinventory.HostProvenance{HostKind: "generic-local", HostID: "test", ProcessID: 1, LoopcoderVersion: "test", Platform: "test"}
			account.PolicyVersion = providerinventory.PolicyVersion
			account.SideEffectClass = "local-read"
			account.Classification = "provider-output-untrusted"
			account.Source = providerinventory.SourceDescriptor{Kind: "fixture", AdapterID: account.AdapterID}
			account.Evidence = providerinventory.EvidenceSummary{Kind: string(providerinventory.EvidenceStatusCommand), CommandBounded: true, NoShell: true}
			account.GapReasons = []string{}
			if _, err := tx.Exec(ctx, `INSERT INTO account_profiles(account_profile_id, adapter_id, provider_installation_id, payload_json)
				VALUES (?, ?, ?, ?) ON CONFLICT(account_profile_id) DO UPDATE SET payload_json = excluded.payload_json`,
				account.AccountProfileID, account.AdapterID, ptrValue(account.ProviderInstallationID), mustJSON(account)); err != nil {
				return err
			}
		}
		for _, auth := range fixture.inventory.AuthReadiness {
			auth.Scope = "machine"
			auth.ProjectID = nil
			if auth.ReadinessState == "" {
				auth.ReadinessState = providerinventory.ReadinessReady
			}
			if auth.ReadinessConfidence == "" {
				auth.ReadinessConfidence = providerinventory.ConfidenceExact
			}
			if auth.EvidenceKind == "" {
				auth.EvidenceKind = providerinventory.EvidenceStatusCommand
			}
			if auth.AuthorizationScopeState == "" {
				auth.AuthorizationScopeState = providerinventory.AuthorizationAllKnown
			}
			auth.CreatedAt = firstNonEmpty(auth.CreatedAt, at)
			auth.UpdatedAt = firstNonEmpty(auth.UpdatedAt, at)
			auth.CreatedBy = providerinventory.ActorProvenance{ActorKind: "policy-engine", ActorID: "test", DecisionAuthority: "deterministic-policy-engine", Source: "test"}
			auth.UpdatedBy = auth.CreatedBy
			auth.Host = providerinventory.HostProvenance{HostKind: "generic-local", HostID: "test", ProcessID: 1, LoopcoderVersion: "test", Platform: "test"}
			auth.PolicyVersion = providerinventory.PolicyVersion
			auth.SideEffectClass = "local-read"
			auth.Classification = "provider-output-untrusted"
			auth.Source = providerinventory.SourceDescriptor{Kind: "fixture", AdapterID: auth.AdapterID}
			auth.Evidence = providerinventory.EvidenceSummary{Kind: string(providerinventory.EvidenceStatusCommand), CommandBounded: true, NoShell: true}
			auth.GapReasons = []string{}
			if _, err := tx.Exec(ctx, `INSERT INTO auth_readiness(auth_readiness_id, adapter_id, provider_installation_id, account_profile_id, payload_json)
				VALUES (?, ?, ?, ?, ?) ON CONFLICT(auth_readiness_id) DO UPDATE SET payload_json = excluded.payload_json`,
				auth.AuthReadinessID, auth.AdapterID, ptrValue(auth.ProviderInstallationID), ptrValue(auth.AccountProfileID), mustJSON(auth)); err != nil {
				return err
			}
		}
		for _, model := range fixture.inventory.ModelCapabilities {
			if model.ReadOnly == "" {
				model.ReadOnly = providerinventory.CapabilityFalse
			}
			if model.JSONOutput == "" {
				model.JSONOutput = providerinventory.CapabilityFalse
			}
			if model.NestedSubagents == "" {
				model.NestedSubagents = providerinventory.CapabilityFalse
			}
			if model.MCPConfig == "" {
				model.MCPConfig = providerinventory.CapabilityFalse
			}
			if model.Cancellation == "" {
				model.Cancellation = providerinventory.CapabilityFalse
			}
			if model.TokenUsageReporting == "" {
				model.TokenUsageReporting = providerinventory.CapabilityFalse
			}
			if model.ImageInput == "" {
				model.ImageInput = providerinventory.CapabilityFalse
			}
			if model.ImageOutput == "" {
				model.ImageOutput = providerinventory.CapabilityFalse
			}
			model.Constraints = []string{}
			model.EntrySources = []providerinventory.CatalogEntrySource{}
			model.Conflicts = []providerinventory.CatalogConflict{}
			model.CreatedAt = firstNonEmpty(model.CreatedAt, at)
			model.UpdatedAt = firstNonEmpty(model.UpdatedAt, at)
			model.CreatedBy = providerinventory.ActorProvenance{ActorKind: "policy-engine", ActorID: "test", DecisionAuthority: "deterministic-policy-engine", Source: "test"}
			model.UpdatedBy = model.CreatedBy
			model.Host = providerinventory.HostProvenance{HostKind: "generic-local", HostID: "test", ProcessID: 1, LoopcoderVersion: "test", Platform: "test"}
			model.PolicyVersion = providerinventory.PolicyVersion
			model.SideEffectClass = "local-read"
			model.Classification = "provider-output-untrusted"
			model.Source = providerinventory.SourceDescriptor{Kind: "fixture", AdapterID: model.AdapterID}
			model.Evidence = providerinventory.EvidenceSummary{Kind: string(providerinventory.EvidenceStatusCommand), CommandBounded: true, NoShell: true}
			model.GapReasons = []string{}
			if _, err := tx.Exec(ctx, `INSERT INTO model_catalog_snapshots(model_catalog_snapshot_id, adapter_id, provider_installation_id, payload_json)
				VALUES (?, ?, ?, '{}') ON CONFLICT(model_catalog_snapshot_id) DO NOTHING`,
				model.ModelCatalogSnapshotID, model.AdapterID, "pinst-"+model.AdapterID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO model_capabilities(model_capability_id, model_catalog_snapshot_id, adapter_id, payload_json)
				VALUES (?, ?, ?, ?) ON CONFLICT(model_capability_id) DO UPDATE SET payload_json = excluded.payload_json`,
				model.ModelCapabilityID, model.ModelCatalogSnapshotID, model.AdapterID, mustJSON(model)); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO quota_telemetry_sources(
			quota_source_id, schema_version, record_version, adapter_id, source_kind, source_key,
			source_schema_version, network_declared, payload_json)
			VALUES ('qsrc-fixture', ?, 1, 'fixture', ?, 'fixture', 'test', 0, '{}')
			ON CONFLICT(quota_source_id) DO NOTHING`,
			providerinventory.QuotaTelemetrySourceSchema, providerinventory.QuotaSourceFixture); err != nil {
			return err
		}
		for _, snapshot := range fixture.inventory.QuotaSnapshots {
			if _, err := tx.Exec(ctx, `INSERT INTO quota_snapshots(
				quota_snapshot_id, quota_source_id, source_kind, adapter_id, scope_key, quantity_kind, unit,
				window_kind, confidence, freshness_state, captured_at, stale_after, terminal_error_code, payload_json)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(quota_snapshot_id) DO UPDATE SET payload_json = excluded.payload_json`,
				snapshot.QuotaSnapshotID, snapshot.QuotaSourceID, snapshot.SourceKind, snapshot.AdapterID, firstNonEmpty(snapshot.ScopeKey, "provider:"+snapshot.AdapterID),
				snapshot.QuantityKind, snapshot.Unit, snapshot.WindowKind, snapshot.Confidence, snapshot.FreshnessState, snapshot.CapturedAt,
				snapshot.StaleAfter, snapshot.TerminalErrorCode, mustJSON(snapshot)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed cached inventory payloads: %v", err)
	}
}

func TestInputsWithCachedInventoryMissingCacheLeavesQuotaUnknown(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, err := storage.Open(ctx, storage.Options{Path: tempDB(t), Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()
	inputs, err := InputsWithCachedInventory(ctx, store, Inputs{
		Requirement:     workerRequirement("task-missing-cache"),
		RuntimeContract: fixture.contract,
		HostName:        "codex-cli",
		Policy:          Policy{RequireExactQuota: true},
	})
	if err != nil {
		t.Fatalf("InputsWithCachedInventory: %v", err)
	}
	result := FilterHardEligibility(inputs)
	if len(result.Eligible) != 0 || len(inputs.Inventory.QuotaSnapshots) != 0 {
		t.Fatalf("missing cache result eligible=%#v inventory=%#v, want no fabricated quota", result.Eligible, inputs.Inventory.QuotaSnapshots)
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

func TestBuiltInSoulTeraLunaRolesAreProviderNeutralEnvelopes(t *testing.T) {
	roles := BuiltInRoleDefinitions()
	byKey := map[string]RoleDefinition{}
	for _, role := range roles {
		byKey[role.RoleKey] = role
		payload, err := json.Marshal(role)
		if err != nil {
			t.Fatalf("marshal role %s: %v", role.RoleKey, err)
		}
		for _, forbidden := range []string{"gpt-", "claude-", "gemini-", "codex", "opus"} {
			if strings.Contains(strings.ToLower(string(payload)), forbidden) {
				t.Fatalf("built-in role %s contains model/provider identifier %q: %s", role.RoleKey, forbidden, payload)
			}
		}
		for _, want := range []string{"provider_name", "model_id", "account_profile_id"} {
			if !containsString(role.ForbiddenBindings, want) {
				t.Fatalf("built-in role %s forbidden_bindings = %#v, want %s", role.RoleKey, role.ForbiddenBindings, want)
			}
		}
	}
	for _, key := range []string{RoleKeySoul, RoleKeyTera, RoleKeyLuna} {
		role := byKey[key]
		if role.RoleDefinitionID == "" || role.SchemaVersion != RoleDefinitionSchema || len(role.MinimumCapabilities) == 0 {
			t.Fatalf("role %s = %#v, want versioned capability envelope", key, role)
		}
	}
}

func TestCustomRoleDefinitionRoutesWithoutRouterCodeChange(t *testing.T) {
	fixture := newFixture(t)
	req := workerRequirement("task-custom-role")
	req.RoleKey = "docs-auditor"
	req.PermissionRequired = taskrequirements.PermissionReadOnly
	req.RequiredOutput = taskrequirements.OutputVerificationVerdict
	custom := RoleDefinition{
		SchemaVersion:         RoleDefinitionSchema,
		RecordVersion:         1,
		RoleKey:               "docs-auditor",
		RoleVersion:           "1",
		Description:           "custom read-only JSON verifier envelope",
		AllowedRiskTiers:      []taskrequirements.RiskTier{taskrequirements.RiskMedium},
		MinimumCapabilities:   []taskrequirements.CapabilityRequirement{roleCapability(taskrequirements.CapabilityRolesSupported, RoleKeyVerifier), boolCapability(taskrequirements.CapabilityReadOnly), boolCapability(taskrequirements.CapabilityJSONOutput)},
		PermissionFloor:       taskrequirements.PermissionReadOnly,
		PermissionCeiling:     taskrequirements.PermissionReadOnly,
		DefaultOutputContract: taskrequirements.OutputVerificationVerdict,
		MaxSideEffectClass:    taskrequirements.SideEffectProviderLaunch,
		QualityFloor:          taskrequirements.QualityStandard,
		ReasoningDepth:        ReasoningDepthStandard,
		LatencyTolerance:      LatencyToleranceStandard,
		CostTolerance:         CostToleranceStandard,
		PolicyVersion:         RoleDefinitionPolicyVersion,
	}
	candidate := fixture.candidate("codex", "acct-b", "codex-verifier")
	candidate.RoleKey = "docs-auditor"
	candidate.Permission = taskrequirements.PermissionReadOnly

	result := FilterHardEligibility(Inputs{
		Requirement:     req,
		RoleDefinitions: []RoleDefinition{custom},
		Candidates:      []Candidate{candidate},
		Inventory:       fixture.inventory,
		Budgets:         fixture.budgets,
		RuntimeContract: fixture.contract,
		HostName:        "codex-cli",
		Policy:          Policy{EvidencePolicy: EvidenceAllowEstimated},
	})
	if len(result.Eligible) != 1 {
		t.Fatalf("eligible custom role = %#v rejected=%#v, want one candidate", result.Eligible, result.Rejected)
	}
}

func TestConfigCustomRoleConvertsIntoHardEligibilityPath(t *testing.T) {
	fixture := newFixture(t)
	parsed, err := config.Parse([]byte(`
version: 1
role_definitions:
  - schema_version: loopcoder.role_definition.v1
    record_version: 1
    role_key: tool-auditor
    role_version: "1"
    description: Custom worker role requiring tool and context evidence
    allowed_risk_tiers: [medium]
    minimum_capabilities:
      - dimension: roles_supported
        required_value: worker
        minimum_confidence: exact
        freshness_required: fresh
        source: fixture
    permission_floor: read-only
    permission_ceiling: write
    default_output_contract: markdown
    quality_floor: standard
    reasoning_depth: standard
    required_tools: [filesystem-read]
    minimum_context_window_tokens: 120000
    max_side_effect_class: provider-launch
    latency_tolerance: standard
    cost_tolerance: standard
    policy_version: role-definition-v1
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	roles, err := RoleDefinitionsFromConfig(parsed.RoleDefinitions)
	if err != nil {
		t.Fatalf("RoleDefinitionsFromConfig() error = %v", err)
	}
	req := workerRequirement("task-config-role")
	req.RoleKey = "tool-auditor"
	req.PermissionRequired = taskrequirements.PermissionReadOnly
	candidate := fixture.candidate("codex", "acct-a", "codex-good")
	candidate.RoleKey = "tool-auditor"
	candidate.Permission = taskrequirements.PermissionReadOnly

	rejected := FilterHardEligibility(Inputs{
		Requirement:     req,
		RoleDefinitions: roles,
		Candidates:      []Candidate{candidate},
		Inventory:       fixture.inventory,
		Budgets:         fixture.budgets,
		RuntimeContract: fixture.contract,
		HostName:        "codex-cli",
		Policy:          Policy{EvidencePolicy: EvidenceAllowEstimated},
	})
	if len(rejected.Eligible) != 0 {
		t.Fatalf("eligible without context/tool evidence = %#v, want rejected", rejected.Eligible)
	}
	if !rejectedHas(rejected, candidate.RoutingCandidateID, RejectContextWindowInsufficient) || !rejectedHas(rejected, candidate.RoutingCandidateID, RejectToolSupportUnsupported) {
		t.Fatalf("rejections = %#v, want context and tool hard failures", rejected.Rejected)
	}

	fixture.inventory.ModelCapabilities[0].ContextWindowTokens = &providerinventory.CapabilityNumeric{Value: 200000, Confidence: providerinventory.ConfidenceExact}
	fixture.inventory.ModelCapabilities[0].ToolSupport = []providerinventory.CapabilityFact{{Name: "filesystem-read", Value: "supported", Confidence: providerinventory.ConfidenceExact}}
	eligible := FilterHardEligibility(Inputs{
		Requirement:     req,
		RoleDefinitions: roles,
		Candidates:      []Candidate{candidate},
		Inventory:       fixture.inventory,
		Budgets:         fixture.budgets,
		RuntimeContract: fixture.contract,
		HostName:        "codex-cli",
		Policy:          Policy{EvidencePolicy: EvidenceAllowEstimated},
	})
	if len(eligible.Eligible) != 1 {
		t.Fatalf("eligible with context/tool evidence = %#v rejected=%#v, want one candidate", eligible.Eligible, eligible.Rejected)
	}
}

func TestRoleIndependenceRequirementAppliesWithoutWeakeningTask(t *testing.T) {
	fixture := newFixture(t)
	req := verifierRequirement("task-role-independence")
	req.RoleKey = "strict-verifier"
	req.PermissionRequired = taskrequirements.PermissionWrite
	req.VerificationRequirements = nil
	role := RoleDefinition{
		SchemaVersion:            RoleDefinitionSchema,
		RecordVersion:            1,
		RoleKey:                  "strict-verifier",
		RoleVersion:              "1",
		Description:              "Verifier role with provider independence for high risk",
		AllowedRiskTiers:         []taskrequirements.RiskTier{taskrequirements.RiskHigh},
		MinimumCapabilities:      []taskrequirements.CapabilityRequirement{roleCapability(taskrequirements.CapabilityRolesSupported, RoleKeyVerifier), boolCapability(taskrequirements.CapabilityReadOnly), boolCapability(taskrequirements.CapabilityJSONOutput)},
		PermissionFloor:          taskrequirements.PermissionReadOnly,
		PermissionCeiling:        taskrequirements.PermissionReadOnly,
		DefaultOutputContract:    taskrequirements.OutputVerificationVerdict,
		IndependenceRequirements: map[taskrequirements.RiskTier]taskrequirements.IndependenceLevel{taskrequirements.RiskHigh: taskrequirements.IndependenceDifferentProvider},
		QualityFloor:             taskrequirements.QualityStandard,
		ReasoningDepth:           ReasoningDepthAdversarial,
		MaxSideEffectClass:       taskrequirements.SideEffectProviderLaunch,
		LatencyTolerance:         LatencyToleranceStandard,
		CostTolerance:            CostToleranceStandard,
		PolicyVersion:            RoleDefinitionPolicyVersion,
	}
	worker := fixture.candidate("codex", "acct-a", "codex-good")
	candidate := fixture.candidate("codex", "acct-b", "codex-verifier")
	candidate.RoleKey = "strict-verifier"
	candidate.Permission = taskrequirements.PermissionReadOnly

	result := FilterHardEligibility(Inputs{
		Requirement:     req,
		RoleDefinitions: []RoleDefinition{role},
		Candidates:      []Candidate{candidate},
		Inventory:       fixture.inventory,
		Budgets:         fixture.budgets,
		RuntimeContract: fixture.contract,
		HostName:        "codex-cli",
		Policy:          Policy{EvidencePolicy: EvidenceAllowEstimated},
		WorkerRoute:     &worker,
	})
	if len(result.Eligible) != 0 {
		t.Fatalf("eligible same-provider verifier = %#v, want role independence rejection", result.Eligible)
	}
	if !rejectedHas(result, candidate.RoutingCandidateID, RejectVerifierIndependenceInsufficient) {
		t.Fatalf("rejections = %#v, want verifier-independence-insufficient", result.Rejected)
	}
	if !rejectedHas(result, candidate.RoutingCandidateID, RejectPermissionUnsupported) {
		t.Fatalf("rejections = %#v, want task read-only hard floor preserved", result.Rejected)
	}
}

func TestProgrammaticCustomRoleValidationFailsClosed(t *testing.T) {
	fixture := newFixture(t)
	req := workerRequirement("task-invalid-role")
	req.RoleKey = "invalid-role"
	role := RoleDefinition{
		SchemaVersion:         RoleDefinitionSchema,
		RecordVersion:         1,
		RoleKey:               "invalid-role",
		RoleVersion:           "1",
		Description:           "Invalid role",
		AllowedRiskTiers:      []taskrequirements.RiskTier{taskrequirements.RiskMedium},
		MinimumCapabilities:   []taskrequirements.CapabilityRequirement{{Dimension: taskrequirements.CapabilityDimension("unknown_dimension"), RequiredValue: true, MinimumConfidence: providerinventory.ConfidenceExact, FreshnessRequired: providerinventory.FreshnessFresh}},
		PermissionFloor:       taskrequirements.PermissionWrite,
		PermissionCeiling:     taskrequirements.PermissionReadOnly,
		DefaultOutputContract: taskrequirements.OutputPatch,
		QualityFloor:          taskrequirements.QualityStandard,
		ReasoningDepth:        ReasoningDepthStandard,
		MaxSideEffectClass:    taskrequirements.SideEffectProviderLaunch,
		LatencyTolerance:      LatencyToleranceStandard,
		CostTolerance:         CostToleranceStandard,
		PolicyVersion:         RoleDefinitionPolicyVersion,
	}
	candidate := fixture.candidate("codex", "acct-a", "codex-good")
	candidate.RoleKey = "invalid-role"

	result := FilterHardEligibility(Inputs{
		Requirement:     req,
		RoleDefinitions: []RoleDefinition{role},
		Candidates:      []Candidate{candidate},
		Inventory:       fixture.inventory,
		Budgets:         fixture.budgets,
		RuntimeContract: fixture.contract,
		HostName:        "codex-cli",
		Policy:          Policy{EvidencePolicy: EvidenceAllowEstimated},
	})
	if len(result.Eligible) != 0 {
		t.Fatalf("eligible invalid programmatic role = %#v, want fail closed", result.Eligible)
	}
	if !rejectedHas(result, candidate.RoutingCandidateID, RejectRoleUnsupported) {
		t.Fatalf("rejections = %#v, want role-unsupported", result.Rejected)
	}
}

func TestUnknownTaskCapabilityDimensionFailsClosed(t *testing.T) {
	fixture := newFixture(t)
	req := workerRequirement("task-unknown-capability")
	req.RequiredCapabilities = append(req.RequiredCapabilities, taskrequirements.CapabilityRequirement{
		Dimension:         taskrequirements.CapabilityDimension("future_capability"),
		RequiredValue:     true,
		MinimumConfidence: providerinventory.ConfidenceExact,
		FreshnessRequired: providerinventory.FreshnessFresh,
	})
	candidate := fixture.candidate("codex", "acct-a", "codex-good")

	result := FilterHardEligibility(Inputs{
		Requirement:     req,
		Candidates:      []Candidate{candidate},
		Inventory:       fixture.inventory,
		Budgets:         fixture.budgets,
		RuntimeContract: fixture.contract,
		HostName:        "codex-cli",
		Policy:          Policy{EvidencePolicy: EvidenceAllowEstimated},
	})
	if len(result.Eligible) != 0 {
		t.Fatalf("eligible unknown task capability = %#v, want fail closed", result.Eligible)
	}
	if !rejectedHas(result, candidate.RoutingCandidateID, RejectUnknownRecordVersion) {
		t.Fatalf("rejections = %#v, want unknown-record-version", result.Rejected)
	}
}

func TestNestedSubagentPermissionCannotExceedParentDelegation(t *testing.T) {
	fixture := newFixture(t)
	nestedModel := model("codex", "codex-nested", "nested-current", []providerinventory.CatalogRole{providerinventory.CatalogRoleNestedSubagents}, fixture.now, caps{nested: providerinventory.CapabilityTrue, cancellation: providerinventory.CapabilityTrue})
	fixture.inventory.ModelCapabilities = append(fixture.inventory.ModelCapabilities, nestedModel)
	fixture.inventory.QuotaSnapshots = append(fixture.inventory.QuotaSnapshots, quota("qsnap-codex-a-nested", "codex", "pinst-codex", "acct-a", "codex-nested", providerinventory.ConfidenceExact, providerinventory.FreshnessFresh, 100, fixture.now))
	fixture.budgets = append(fixture.budgets, budgetSummary("bpol-codex-nested", "codex", "acct-a", "codex-nested", 100))
	req := workerRequirement("task-nested")
	req.RoleKey = RoleKeyNestedSubagent
	req.PermissionRequired = taskrequirements.PermissionWrite
	req.RequiredCapabilities = []taskrequirements.CapabilityRequirement{{Dimension: taskrequirements.CapabilityCancellation, RequiredValue: true, MinimumConfidence: providerinventory.ConfidenceExact, FreshnessRequired: providerinventory.FreshnessFresh}}
	candidate := fixture.candidate("codex", "acct-a", "codex-nested")
	candidate.RoleKey = RoleKeyNestedSubagent
	candidate.Permission = taskrequirements.PermissionOrchestrate

	result := FilterHardEligibility(Inputs{
		Requirement:     req,
		Candidates:      []Candidate{candidate},
		Inventory:       fixture.inventory,
		Budgets:         fixture.budgets,
		RuntimeContract: fixture.contract,
		HostName:        "codex-cli",
		Policy:          Policy{EvidencePolicy: EvidenceAllowEstimated},
	})
	if len(result.Eligible) != 0 {
		t.Fatalf("eligible nested over-permission = %#v, want rejected", result.Eligible)
	}
	if !rejectedHas(result, candidate.RoutingCandidateID, RejectPermissionUnsupported) {
		t.Fatalf("rejections = %#v, want permission-unsupported", result.Rejected)
	}
}

func TestUnknownRoleCapabilityFailsClosedWithTypedReason(t *testing.T) {
	fixture := newFixture(t)
	req := workerRequirement("task-unknown-role-cap")
	req.RoleKey = RoleKeyLuna
	candidate := fixture.candidate("codex", "acct-a", "codex-good")
	fixture.inventory.ModelCapabilities[0].RolesSupported = nil

	result := FilterHardEligibility(Inputs{
		Requirement:     req,
		Candidates:      []Candidate{candidate},
		Inventory:       fixture.inventory,
		Budgets:         fixture.budgets,
		RuntimeContract: fixture.contract,
		HostName:        "codex-cli",
		Policy:          Policy{EvidencePolicy: EvidenceAllowEstimated},
	})
	if len(result.Eligible) != 0 {
		t.Fatalf("eligible unknown role capability = %#v, want fail closed", result.Eligible)
	}
	if !rejectedHas(result, candidate.RoutingCandidateID, RejectRoleUnsupported) {
		t.Fatalf("rejections = %#v, want role-unsupported", result.Rejected)
	}
}

func TestRoleSelectionCannotLowerTaskHardPermission(t *testing.T) {
	fixture := newFixture(t)
	req := workerRequirement("task-permission-floor")
	req.RoleKey = RoleKeyLuna
	req.PermissionRequired = taskrequirements.PermissionWrite
	candidate := fixture.candidate("codex", "acct-a", "codex-good")
	candidate.RoleKey = RoleKeyLuna
	candidate.Permission = taskrequirements.PermissionReadOnly

	result := FilterHardEligibility(Inputs{
		Requirement:     req,
		Candidates:      []Candidate{candidate},
		Inventory:       fixture.inventory,
		Budgets:         fixture.budgets,
		RuntimeContract: fixture.contract,
		HostName:        "codex-cli",
		Policy:          Policy{EvidencePolicy: EvidenceAllowEstimated},
	})
	if len(result.Eligible) != 0 {
		t.Fatalf("eligible = %#v, want role not to lower write permission task", result.Eligible)
	}
	if !rejectedHas(result, candidate.RoutingCandidateID, RejectPermissionUnsupported) {
		t.Fatalf("rejections = %#v, want permission-unsupported", result.Rejected)
	}
}

func TestModelRenameRemovalChangesCandidatesNotRoleIdentity(t *testing.T) {
	fixture := newFixture(t)
	req := workerRequirement("task-rename")
	req.RoleKey = RoleKeyTera
	oldModel := fixture.inventory.ModelCapabilities[0]
	newModel := oldModel
	newModel.ModelCapabilityID = "codex-renamed"
	newModel.CanonicalModelID = "renamed-current-id"
	oldModel.LifecycleState = providerinventory.LifecycleRemoved
	oldModel.AvailabilityState = providerinventory.AvailabilityRemoved
	fixture.inventory.ModelCapabilities = []providerinventory.ModelCapability{oldModel, newModel}
	role, ok := ResolveRoleDefinition(RoleKeyTera, nil)
	if !ok {
		t.Fatal("tera role definition missing")
	}

	result := FilterHardEligibility(Inputs{
		Requirement:     req,
		Inventory:       fixture.inventory,
		Budgets:         fixture.budgets,
		RuntimeContract: fixture.contract,
		HostName:        "codex-cli",
		Policy:          Policy{EvidencePolicy: EvidenceAllowEstimated},
	})
	if len(result.Eligible) == 0 {
		t.Fatalf("eligible candidates = %#v rejected=%#v, want renamed current model candidates", result.Eligible, result.Rejected)
	}
	for _, candidate := range result.Eligible {
		if candidate.ModelCapabilityID != "codex-renamed" {
			t.Fatalf("eligible candidates = %#v rejected=%#v, want no removed model candidates", result.Eligible, result.Rejected)
		}
	}
	again, ok := ResolveRoleDefinition(RoleKeyTera, nil)
	if !ok || again.RoleDefinitionID != role.RoleDefinitionID {
		t.Fatalf("role identity changed after model catalog change: before=%#v after=%#v", role, again)
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

func withGrokOrdinaryWorkerFixture(f hardFixture) hardFixture {
	projectID := "proj-routing"
	f.inventory.Installations = append(f.inventory.Installations,
		installation(projectID, "pinst-grok", "grok", f.now, providerinventory.FreshnessFresh, providerinventory.ConfidenceExact),
	)
	f.inventory.AccountProfiles = append(f.inventory.AccountProfiles,
		account(projectID, "acct-grok", "grok", "pinst-grok", f.now, providerinventory.FreshnessFresh, providerinventory.ConfidenceExact),
	)
	f.inventory.AuthReadiness = append(f.inventory.AuthReadiness,
		auth(projectID, "auth-acct-grok", "grok", "pinst-grok", "acct-grok", f.now, providerinventory.FreshnessFresh, providerinventory.ConfidenceExact),
	)
	f.inventory.ModelCapabilities = append(f.inventory.ModelCapabilities,
		model("grok", "grok-good", "grok-4.5", []providerinventory.CatalogRole{providerinventory.CatalogRoleWorker}, f.now, goodCaps()),
	)
	f.inventory.QuotaSnapshots = append(f.inventory.QuotaSnapshots,
		quota("qsnap-grok-good", "grok", "pinst-grok", "acct-grok", "grok-good", providerinventory.ConfidenceExact, providerinventory.FreshnessFresh, 900, f.now),
	)
	f.budgets = append(f.budgets,
		budgetSummary("bpol-grok", "grok", "acct-grok", "grok-good", 100),
	)
	f.contract.Providers = append(f.contract.Providers, providerRuntime("grok", true, false, true))
	return f
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

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
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
