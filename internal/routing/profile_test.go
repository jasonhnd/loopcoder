package routing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

func TestBuiltInRoutingPolicyProfilesAreProviderNeutralAndVersioned(t *testing.T) {
	profiles := BuiltInRoutingPolicyProfiles(time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC))
	if len(profiles) != 3 {
		t.Fatalf("profiles = %d, want fast/balanced/deep", len(profiles))
	}
	seen := map[string]bool{}
	for _, profile := range profiles {
		if err := ValidateRoutingPolicyProfile(profile); err != nil {
			t.Fatalf("ValidateRoutingPolicyProfile(%s): %v", profile.ProfileKey, err)
		}
		seen[profile.ProfileKey] = true
		payload, err := delivery.CanonicalJSON(profile)
		if err != nil {
			t.Fatalf("canonical profile %s: %v", profile.ProfileKey, err)
		}
		for _, forbidden := range []string{"gpt-", "claude-", "gemini-", "codex", "opus"} {
			if strings.Contains(strings.ToLower(string(payload)), forbidden) {
				t.Fatalf("profile %s contains provider/model binding %q: %s", profile.ProfileKey, forbidden, payload)
			}
		}
		if profile.BudgetSettings.AllowPaidOverage {
			t.Fatalf("profile %s allows paid overage", profile.ProfileKey)
		}
		if profile.ProfileKey == ProfileKeyBalanced {
			if profile.OptimizationPolicy.StrategyKey != StrategyBalanced ||
				profile.OptimizationPolicy.TargetUtilizationBP != DefaultTargetUtilization ||
				profile.OptimizationPolicy.CompletionReserveBP != 500 ||
				profile.OptimizationPolicy.VerificationReserveBP != 800 ||
				profile.OptimizationPolicy.AllowPaidOverage {
				t.Fatalf("balanced profile strategy defaults = %#v", profile.OptimizationPolicy)
			}
		}
	}
	for _, key := range []string{ProfileKeyFast, ProfileKeyBalanced, ProfileKeyDeep} {
		if !seen[key] {
			t.Fatalf("missing profile %s", key)
		}
	}
}

func TestRoutingPolicyProfilePersistenceIsReplaySafeAndVersioned(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, err := storage.Open(ctx, storage.Options{Path: tempDB(t), Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	defer store.Close()
	seedRoutingDecisionStore(t, ctx, store, fixture.now)
	if _, err := EnsureBuiltInRoleDefinitions(ctx, store, fixture.now); err != nil {
		t.Fatalf("EnsureBuiltInRoleDefinitions: %v", err)
	}

	balanced := BalancedRoutingPolicyProfile(fixture.now)
	first, err := PersistRoutingPolicyProfile(ctx, store, balanced)
	if err != nil {
		t.Fatalf("PersistRoutingPolicyProfile first: %v", err)
	}
	replayed, err := PersistRoutingPolicyProfile(ctx, store, balanced)
	if err != nil {
		t.Fatalf("PersistRoutingPolicyProfile replay: %v", err)
	}
	if first.RoutingPolicyProfileID != replayed.RoutingPolicyProfileID || first.PolicyFingerprint != replayed.PolicyFingerprint {
		t.Fatalf("profile replay changed record: first=%#v replay=%#v", first, replayed)
	}

	changed := balanced
	changed.ProfileVersion = "2"
	changed.RoutingPolicyProfileID = ""
	changed.PolicyFingerprint = ""
	changed.OptimizationPolicy.Weights = map[ComponentName]int{
		ComponentAvailability: 25, ComponentQuotaHeadroom: 20, ComponentQualityFit: 25, ComponentLatency: 10, ComponentCost: 10, ComponentDiversity: 10,
	}
	second, err := PersistRoutingPolicyProfile(ctx, store, changed)
	if err != nil {
		t.Fatalf("PersistRoutingPolicyProfile changed version: %v", err)
	}
	if first.RoutingPolicyProfileID == second.RoutingPolicyProfileID || first.PolicyFingerprint == second.PolicyFingerprint {
		t.Fatalf("profile version change did not create new immutable identity: first=%#v second=%#v", first, second)
	}
	loadedFirst, err := LoadRoutingPolicyProfile(ctx, store, first.RoutingPolicyProfileID)
	if err != nil {
		t.Fatalf("LoadRoutingPolicyProfile first: %v", err)
	}
	if loadedFirst.PolicyFingerprint != first.PolicyFingerprint {
		t.Fatalf("first profile history was rewritten: got %s want %s", loadedFirst.PolicyFingerprint, first.PolicyFingerprint)
	}
}

func TestRoutingPolicyProfileRejectsMutatedPayloadWithTrustedFingerprint(t *testing.T) {
	fixture := newFixture(t)
	profile := BalancedRoutingPolicyProfile(fixture.now)
	mutated := profile
	mutated.GraphBounds.MaxTasks++
	if err := ValidateRoutingPolicyProfile(mutated); !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("ValidateRoutingPolicyProfile error = %v, want ErrRoutingFingerprintMismatch", err)
	}
	input := replayDecisionInput(fixture)
	input.RoutingPolicyProfile = mutated
	if _, err := BuildRoutingDecision(input); !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("BuildRoutingDecision error = %v, want ErrRoutingFingerprintMismatch", err)
	}
}

func TestEnsureBuiltInRoutingPolicyProfilesIsRestartIdempotentAcrossClocks(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	path := tempDB(t)
	store, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	seedRoutingDecisionStore(t, ctx, store, fixture.now)
	first, err := EnsureBuiltInRoutingPolicyProfiles(ctx, store, fixture.now)
	if err != nil {
		t.Fatalf("EnsureBuiltInRoutingPolicyProfiles first: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close first store: %v", err)
	}
	later := fixture.now.Add(48 * time.Hour)
	reopened, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return later }})
	if err != nil {
		t.Fatalf("reopen storage: %v", err)
	}
	defer reopened.Close()
	second, err := EnsureBuiltInRoutingPolicyProfiles(ctx, reopened, later)
	if err != nil {
		t.Fatalf("EnsureBuiltInRoutingPolicyProfiles second: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("profile count changed: first=%d second=%d", len(first), len(second))
	}
	for i := range first {
		if first[i].RoutingPolicyProfileID != second[i].RoutingPolicyProfileID || first[i].PolicyFingerprint != second[i].PolicyFingerprint || first[i].EffectiveFrom != second[i].EffectiveFrom {
			t.Fatalf("built-in profile changed across restart:\nfirst=%#v\nsecond=%#v", first[i], second[i])
		}
	}
}

func TestValidatePolicyInputsRejectsInvalidAndStalePins(t *testing.T) {
	fixture := newFixture(t)
	diagnostics := ValidatePolicyInputs(fixture.inventory, []Pin{{
		PinID:             "pin-stale",
		AdapterID:         "codex",
		AccountProfileID:  "acct-stale",
		ModelCapabilityID: "missing-model",
	}}, []Exclusion{{
		ExclusionID: "exclude-wrong-adapter",
		AdapterID:   "missing-provider",
	}}, testFingerprint("policy"), fixture.now)

	for _, want := range []taskrequirements.ErrorCode{
		taskrequirements.ErrMissingReferenceCode,
		taskrequirements.ErrorCode(delivery.ErrStaleApprovalCode),
	} {
		if !diagnosticHasCode(diagnostics, want) {
			t.Fatalf("diagnostics = %#v, want %s", diagnostics, want)
		}
	}
}

func TestPolicyInputPersistenceFailsInvalidPinWithActionableDiagnostic(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, err := storage.Open(ctx, storage.Options{Path: tempDB(t), Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	defer store.Close()
	seedRoutingDecisionStore(t, ctx, store, fixture.now)
	profile := BalancedRoutingPolicyProfile(fixture.now)
	diagnostics := ValidatePolicyInputs(fixture.inventory, []Pin{{PinID: "pin-missing", ModelCapabilityID: "missing-model"}}, nil, profile.PolicyFingerprint, fixture.now)
	_, err = PersistPolicyInput(ctx, store, PolicyInputRecord{
		InputKind:              PolicyInputKindPin,
		ProjectID:              "proj-routing",
		DeliveryRunID:          "drun-routing",
		RoutingPolicyProfileID: profile.RoutingPolicyProfileID,
		PolicyFingerprint:      profile.PolicyFingerprint,
		Scope:                  "task:task-a",
		Reason:                 "operator requested a model pin for this task",
		Constraint:             CandidateConstraint{ModelCapabilityID: "missing-model"},
		ValidationStatus:       ValidationStatusInvalid,
		Diagnostics:            diagnostics,
		Actor:                  delivery.Actor{ActorKind: "user", ActorID: "user-1", DecisionAuthority: "user", Source: "test"},
		Host:                   routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrMissingReference) {
		t.Fatalf("PersistPolicyInput error = %v, want ErrMissingReference", err)
	}
	if len(diagnostics) == 0 || diagnostics[0].Action == "" {
		t.Fatalf("diagnostics not actionable: %#v", diagnostics)
	}
}

func TestOverrideProvenanceCannotBypassHardGates(t *testing.T) {
	diagnostics := ValidateOverrideProvenance([]OverrideProvenance{{
		OverrideID:               "ovr-permission",
		OverrideKind:             "routing",
		Reason:                   "try anyway",
		Scope:                    "permission write release-gate",
		ExpiresAt:                "2026-07-14T00:00:00Z",
		PolicyFingerprint:        testFingerprint("policy"),
		AuthorizationFingerprint: testFingerprint("auth"),
		Actor:                    delivery.Actor{ActorKind: "user", ActorID: "user-1", DecisionAuthority: "user", Source: "test"},
		Host:                     routingHost(),
	}}, time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC))
	if !diagnosticHasCode(diagnostics, taskrequirements.ErrorCode(delivery.ErrPolicyDeniedCode)) {
		t.Fatalf("diagnostics = %#v, want hard-gate policy denial", diagnostics)
	}
}

func TestManualResetAndUnavailableOverridesAreBoundedAndExpiring(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	for _, override := range []OverrideProvenance{
		{
			OverrideID:               "ovr-manual-reset",
			OverrideKind:             "manual-reset",
			Reason:                   "operator cleared a stale cooldown after fresh capacity event",
			Scope:                    "manual-reset task:task-a",
			ExpiresAt:                now.Add(10 * time.Minute).Format(time.RFC3339Nano),
			Actor:                    delivery.Actor{ActorKind: "user", ActorID: "local-user", DecisionAuthority: "user", Source: "test"},
			Host:                     routingHost(),
			PolicyFingerprint:        testFingerprint("policy"),
			AuthorizationFingerprint: testFingerprint("auth"),
			Source:                   "test",
		},
		{
			OverrideID:               "ovr-unavailable",
			OverrideKind:             "manual-unavailable-until",
			Reason:                   "operator marked candidate unavailable until reset",
			Scope:                    "manual-unavailable-until run:drun-routing",
			ExpiresAt:                now.Add(10 * time.Minute).Format(time.RFC3339Nano),
			Actor:                    delivery.Actor{ActorKind: "user", ActorID: "local-user", DecisionAuthority: "user", Source: "test"},
			Host:                     routingHost(),
			PolicyFingerprint:        testFingerprint("policy"),
			AuthorizationFingerprint: testFingerprint("auth"),
			Source:                   "test",
		},
	} {
		if diagnostics := ValidateOverrideProvenance([]OverrideProvenance{override}, now, testFingerprint("policy"), testFingerprint("auth")); len(diagnostics) != 0 {
			t.Fatalf("override %s diagnostics = %#v, want valid bounded override", override.OverrideID, diagnostics)
		}
		override.ExpiresAt = now.Add(-time.Minute).Format(time.RFC3339Nano)
		if diagnostics := ValidateOverrideProvenance([]OverrideProvenance{override}, now, testFingerprint("policy"), testFingerprint("auth")); len(diagnostics) == 0 {
			t.Fatalf("override %s with expired unavailable/reset override unexpectedly valid", override.OverrideID)
		}
	}
}

func TestOverrideProvenanceRequiresExactActiveBindingsThroughRouting(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, err := storage.Open(ctx, storage.Options{Path: tempDB(t), Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	defer store.Close()
	seedRoutingDecisionStore(t, ctx, store, fixture.now)

	profile := BalancedRoutingPolicyProfile(fixture.now)
	input := replayDecisionInput(fixture)
	input.RoutingPolicyProfile = profile
	input.AuthorizationFingerprint = testFingerprint("auth")
	input.OverrideProvenance = []OverrideProvenance{{
		OverrideID:               "ovr-routing",
		OverrideKind:             "routing",
		Reason:                   "prefer a bounded route",
		Scope:                    "routing-preference task-a",
		ExpiresAt:                "2026-07-14T00:00:00Z",
		PolicyFingerprint:        testFingerprint("wrong-policy-but-well-formed"),
		AuthorizationFingerprint: testFingerprint("auth"),
		Actor:                    delivery.Actor{ActorKind: "user", ActorID: "user-1", DecisionAuthority: "user", Source: "test"},
		Host:                     routingHost(),
	}}
	if _, err := BuildRoutingDecision(input); !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("BuildRoutingDecision wrong policy error = %v, want ErrRoutingFingerprintMismatch", err)
	}
	input.OverrideProvenance[0].PolicyFingerprint = profile.PolicyFingerprint
	input.OverrideProvenance[0].AuthorizationFingerprint = testFingerprint("wrong-auth-but-well-formed")
	if _, err := DecideAndPersistRoute(ctx, store, input); !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("DecideAndPersistRoute wrong auth error = %v, want ErrRoutingFingerprintMismatch", err)
	}
}

func TestLegacyModelMigrationMapsOnlyDeterministicSettings(t *testing.T) {
	fixture := newFixture(t)
	mappings := LegacyModelMappingsFromConfig("proj-routing", config.Config{
		Adapters: config.Adapters{Worker: "codex", Verifier: "codex"},
		Worker:   config.Worker{Model: "gpt-5.5", ReasoningEffort: "high"},
		Verifier: config.Verifier{Model: "missing-review-model", ReasoningEffort: "xhigh"},
	}, fixture.inventory, testFingerprint("policy"), fixture.now)
	if len(mappings) != 2 {
		t.Fatalf("mappings = %#v, want worker and verifier entries", mappings)
	}
	if mappings[0].RoleKey != RoleKeyWorker || mappings[0].MappingStatus != "mapped" || mappings[0].MappedModelCapabilityID != "codex-good" {
		t.Fatalf("worker mapping = %#v, want deterministic codex-good", mappings[0])
	}
	if mappings[1].RoleKey != RoleKeyVerifier || mappings[1].MappingStatus != "unresolved" || mappings[1].DiagnosticCode == "" {
		t.Fatalf("verifier mapping = %#v, want typed unresolved", mappings[1])
	}
}

func TestProfileAwareExplainNamesProfileAndSafeOverrideProvenance(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, err := storage.Open(ctx, storage.Options{Path: tempDB(t), Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	defer store.Close()
	seedRoutingDecisionStore(t, ctx, store, fixture.now)

	input := replayDecisionInput(fixture)
	profile := BalancedRoutingPolicyProfile(fixture.now)
	input.RoutingPolicyProfile = profile
	input.AuthorizationFingerprint = testFingerprint("auth")
	input.OverrideProvenance = []OverrideProvenance{{
		OverrideID:               "ovr-routing",
		OverrideKind:             "routing",
		Reason:                   "prefer stored route for this task",
		Scope:                    "routing-preference task-a",
		ExpiresAt:                "2026-07-14T00:00:00Z",
		PolicyFingerprint:        profile.PolicyFingerprint,
		AuthorizationFingerprint: testFingerprint("auth"),
		Actor:                    delivery.Actor{ActorKind: "user", ActorID: "user-1", DecisionAuthority: "user", Source: "test"},
		Host:                     routingHost(),
	}}
	decision, err := DecideAndPersistRoute(ctx, store, input)
	if err != nil {
		t.Fatalf("DecideAndPersistRoute: %v", err)
	}
	loaded, err := LoadRoutingDecision(ctx, store, decision.RoutingDecisionID)
	if err != nil {
		t.Fatalf("LoadRoutingDecision: %v", err)
	}
	if loaded.RoutingPolicyProfile == nil || len(loaded.OverrideProvenance) != 1 {
		t.Fatalf("stored profile-aware decision lost provenance: %#v", loaded)
	}
	human := ExplainHuman(decision)
	for _, want := range []string{"profile balanced-v1 version 1", "override ovr-routing", "prefer stored route"} {
		if !strings.Contains(human, want) {
			t.Fatalf("human explain missing %q:\n%s", want, human)
		}
	}
	stable, err := ExplainJSON(decision)
	if err != nil {
		t.Fatalf("ExplainJSON: %v", err)
	}
	if !strings.Contains(string(stable), `"routing_policy_profile"`) || !strings.Contains(string(stable), `"override_provenance"`) {
		t.Fatalf("stable explain JSON missing profile/override provenance: %s", stable)
	}
}

func TestDecideAndPersistRouteRequiresPersistedCurrentPolicyInputs(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, err := storage.Open(ctx, storage.Options{Path: tempDB(t), Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	defer store.Close()
	seedRoutingDecisionStore(t, ctx, store, fixture.now)
	profile := BalancedRoutingPolicyProfile(fixture.now)
	if _, err := EnsureBuiltInRoleDefinitions(ctx, store, fixture.now); err != nil {
		t.Fatalf("EnsureBuiltInRoleDefinitions: %v", err)
	}
	if _, err := PersistRoutingPolicyProfile(ctx, store, profile); err != nil {
		t.Fatalf("PersistRoutingPolicyProfile: %v", err)
	}

	input := replayDecisionInput(fixture)
	input.RoutingPolicyProfile = profile
	input.Inputs.Pins = []Pin{{PinID: "caller-only", ModelCapabilityID: "codex-good"}}
	if _, err := DecideAndPersistRoute(ctx, store, input); !errors.Is(err, taskrequirements.ErrMissingReference) {
		t.Fatalf("caller-only pin error = %v, want ErrMissingReference", err)
	}

	stored, err := PersistPolicyInput(ctx, store, PolicyInputRecord{
		InputKind:              PolicyInputKindPin,
		ProjectID:              input.ProjectID,
		DeliveryRunID:          input.DeliveryRunID,
		RoutingPolicyProfileID: profile.RoutingPolicyProfileID,
		PolicyFingerprint:      profile.PolicyFingerprint,
		Scope:                  "task:task-a",
		Reason:                 "operator pins this task to the codex-good model",
		Constraint:             CandidateConstraint{ModelCapabilityID: "codex-good"},
		Actor:                  delivery.Actor{ActorKind: "user", ActorID: "user-1", DecisionAuthority: "user", Source: "test"},
		Host:                   routingHost(),
	})
	if err != nil {
		t.Fatalf("PersistPolicyInput current pin: %v", err)
	}
	input.DecisionKey = "route-stored-pin"
	input.Inputs.Pins = nil
	decision, err := DecideAndPersistRoute(ctx, store, input)
	if err != nil {
		t.Fatalf("DecideAndPersistRoute with stored pin: %v", err)
	}
	if len(decision.PolicyInputRecords) != 1 || decision.PolicyInputRecords[0].RoutingPolicyInputID != stored.RoutingPolicyInputID || decision.ChosenCandidateID == "" {
		t.Fatalf("decision did not load stored policy input: %#v", decision)
	}

	staleInput := replayDecisionInput(fixture)
	staleInput.DecisionKey = "route-stale-pin"
	staleInput.RoutingPolicyProfile = profile
	_, err = PersistPolicyInput(ctx, store, PolicyInputRecord{
		InputKind:              PolicyInputKindExclusion,
		ProjectID:              staleInput.ProjectID,
		DeliveryRunID:          staleInput.DeliveryRunID,
		RoutingPolicyProfileID: profile.RoutingPolicyProfileID,
		PolicyFingerprint:      testFingerprint("stale-policy"),
		Scope:                  "task:task-a",
		Reason:                 "stale exclusion should fail before scoring",
		Constraint:             CandidateConstraint{ModelCapabilityID: "claude-good"},
		Actor:                  delivery.Actor{ActorKind: "user", ActorID: "user-2", DecisionAuthority: "user", Source: "test"},
		Host:                   routingHost(),
	})
	if err != nil {
		t.Fatalf("PersistPolicyInput stale exclusion setup: %v", err)
	}
	if _, err := DecideAndPersistRoute(ctx, store, staleInput); !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("stale stored input error = %v, want ErrRoutingFingerprintMismatch", err)
	}
}

func TestDecideAndPersistRouteRequiresPersistedProfileBeforeScoring(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, err := storage.Open(ctx, storage.Options{Path: tempDB(t), Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	defer store.Close()
	seedRoutingDecisionStore(t, ctx, store, fixture.now)
	profile := BalancedRoutingPolicyProfile(fixture.now)
	deleteStoredRoutingPolicyProfile(t, ctx, store, profile.RoutingPolicyProfileID)

	input := replayDecisionInput(fixture)
	input.RoutingPolicyProfileID = profile.RoutingPolicyProfileID
	if _, err := DecideAndPersistRoute(ctx, store, input); !errors.Is(err, taskrequirements.ErrMissingReference) {
		t.Fatalf("missing profile error = %v, want ErrMissingReference", err)
	}
}

func TestDecideAndPersistRouteRejectsStoredProfileMutatedWithOldFingerprint(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, err := storage.Open(ctx, storage.Options{Path: tempDB(t), Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	defer store.Close()
	seedRoutingDecisionStore(t, ctx, store, fixture.now)
	profile := BalancedRoutingPolicyProfile(fixture.now)
	mutated := profile
	mutated.GraphBounds.MaxTasks++
	replaceStoredRoutingPolicyProfilePayload(t, ctx, store, mutated)

	input := replayDecisionInput(fixture)
	input.RoutingPolicyProfileID = profile.RoutingPolicyProfileID
	if _, err := DecideAndPersistRoute(ctx, store, input); !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("mutated stored profile error = %v, want ErrRoutingFingerprintMismatch", err)
	}
}

func TestDecideAndPersistRouteRejectsCallerProfileMutatedWithRecomputedFingerprint(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, err := storage.Open(ctx, storage.Options{Path: tempDB(t), Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	defer store.Close()
	seedRoutingDecisionStore(t, ctx, store, fixture.now)
	stored := BalancedRoutingPolicyProfile(fixture.now)
	mutated := stored
	mutated.GraphBounds.MaxTasks++
	mutated.PolicyFingerprint = ""
	mutated = normalizeRoutingPolicyProfile(mutated)
	if err := ValidateRoutingPolicyProfile(mutated); err != nil {
		t.Fatalf("mutated profile setup should be internally valid: %v", err)
	}

	input := replayDecisionInput(fixture)
	input.RoutingPolicyProfile = mutated
	if _, err := DecideAndPersistRoute(ctx, store, input); !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("caller-mutated profile error = %v, want ErrRoutingFingerprintMismatch", err)
	}
}

func TestDecideAndPersistRouteIDOnlyLoadsStoredProfileSnapshot(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, err := storage.Open(ctx, storage.Options{Path: tempDB(t), Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	defer store.Close()
	seedRoutingDecisionStore(t, ctx, store, fixture.now)
	stored, err := LoadRoutingPolicyProfile(ctx, store, BalancedRoutingPolicyProfile(fixture.now).RoutingPolicyProfileID)
	if err != nil {
		t.Fatalf("LoadRoutingPolicyProfile: %v", err)
	}

	input := replayDecisionInput(fixture)
	input.RoutingPolicyProfileID = stored.RoutingPolicyProfileID
	decision, err := DecideAndPersistRoute(ctx, store, input)
	if err != nil {
		t.Fatalf("DecideAndPersistRoute id-only: %v", err)
	}
	if decision.RoutingPolicyProfile == nil {
		t.Fatalf("decision missing stored routing policy profile snapshot: %#v", decision)
	}
	wantPayload, err := delivery.CanonicalJSON(stored)
	if err != nil {
		t.Fatalf("canonical stored profile: %v", err)
	}
	gotPayload, err := delivery.CanonicalJSON(*decision.RoutingPolicyProfile)
	if err != nil {
		t.Fatalf("canonical decision profile: %v", err)
	}
	if string(gotPayload) != string(wantPayload) || decision.PolicyFingerprint != stored.PolicyFingerprint || decision.OptimizationPolicy.PolicyFingerprint != stored.OptimizationPolicy.PolicyFingerprint {
		t.Fatalf("decision did not use exact stored profile snapshot:\nwant=%s\n got=%s\ndecision=%#v", wantPayload, gotPayload, decision)
	}
	loaded, err := LoadRoutingDecision(ctx, store, decision.RoutingDecisionID)
	if err != nil {
		t.Fatalf("LoadRoutingDecision: %v", err)
	}
	loadedPayload, err := delivery.CanonicalJSON(*loaded.RoutingPolicyProfile)
	if err != nil {
		t.Fatalf("canonical loaded decision profile: %v", err)
	}
	if string(loadedPayload) != string(wantPayload) {
		t.Fatalf("persisted decision profile snapshot changed:\nwant=%s\n got=%s", wantPayload, loadedPayload)
	}
}

func TestRoleDefinitionPersistenceReplayConflictAndProfileReferences(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, err := storage.Open(ctx, storage.Options{Path: tempDB(t), Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	defer store.Close()
	seedRoutingDecisionStore(t, ctx, store, fixture.now)
	role := BuiltInRoleDefinitions()[0]
	first, err := PersistRoleDefinition(ctx, store, role, fixture.now)
	if err != nil {
		t.Fatalf("PersistRoleDefinition first: %v", err)
	}
	replayed, err := PersistRoleDefinition(ctx, store, role, fixture.now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("PersistRoleDefinition replay: %v", err)
	}
	if first.RoleDefinitionID != replayed.RoleDefinitionID {
		t.Fatalf("role replay changed identity: first=%#v replay=%#v", first, replayed)
	}
	conflict := role
	conflict.Description = "conflicting payload"
	if _, err := PersistRoleDefinition(ctx, store, conflict, fixture.now); !errors.Is(err, taskrequirements.ErrDuplicateRecord) {
		t.Fatalf("conflicting role error = %v, want ErrDuplicateRecord", err)
	}
	profile := BalancedRoutingPolicyProfile(fixture.now)
	profile.RoleDefinitionIDs = append(profile.RoleDefinitionIDs, "roledef_missing")
	profile.PolicyFingerprint = ""
	profile = normalizeRoutingPolicyProfile(profile)
	if _, err := PersistRoutingPolicyProfile(ctx, store, profile); !errors.Is(err, taskrequirements.ErrMissingReference) {
		t.Fatalf("missing role profile error = %v, want ErrMissingReference", err)
	}
}

func diagnosticHasCode(diagnostics []PolicyDiagnostic, code taskrequirements.ErrorCode) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}

func deleteStoredRoutingPolicyProfile(t *testing.T, ctx context.Context, store storage.Store, id string) {
	t.Helper()
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM routing_policy_profiles WHERE routing_policy_profile_id = ?`, id)
		return err
	}); err != nil {
		t.Fatalf("delete stored routing policy profile: %v", err)
	}
}

func replaceStoredRoutingPolicyProfilePayload(t *testing.T, ctx context.Context, store storage.Store, profile RoutingPolicyProfile) {
	t.Helper()
	payload, err := delivery.CanonicalJSON(profile)
	if err != nil {
		t.Fatalf("canonical mutated profile: %v", err)
	}
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE routing_policy_profiles SET payload_json = ?, policy_fingerprint = ? WHERE routing_policy_profile_id = ?`, string(payload), profile.PolicyFingerprint, profile.RoutingPolicyProfileID)
		return err
	}); err != nil {
		t.Fatalf("replace stored routing policy profile payload: %v", err)
	}
}
