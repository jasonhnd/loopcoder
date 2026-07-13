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

func diagnosticHasCode(diagnostics []PolicyDiagnostic, code taskrequirements.ErrorCode) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}
