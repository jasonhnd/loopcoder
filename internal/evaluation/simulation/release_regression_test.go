package simulation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

const (
	releaseRegressionFixtureSchemaV1 = "loopcoder.evaluation.release_regression_fixture.v1"
	releaseRegressionPolicyVersion   = "simulation-release-policy-v1"
	updateReleaseGoldensEnv          = "LOOPCODER_UPDATE_RELEASE_REGRESSION_GOLDENS"
)

var issue714ReleaseInvariantIDs = []string{
	"issue-714-decision-ownership-boundaries",
	"issue-714-deterministic-simulations-cover-release-failures",
	"issue-714-durable-reproducible-consequential-decisions",
	"issue-714-host-provider-independent-worker-verifier-selection",
	"issue-714-human-json-inspection-explains-decisions",
	"issue-714-no-fabricated-quota-or-quota-first-unsafe-selection",
	"issue-714-no-uncontrolled-child-recursion",
	"issue-714-recovery-replay-does-not-duplicate-side-effects",
	"issue-714-security-redaction-least-privilege-bounded-side-effects",
	"issue-714-truthful-degrade-for-unsupported-push-wake",
}

type releaseRegressionMatrix struct {
	FixtureSchemaVersion string                       `json:"fixture_schema_version"`
	PolicySchemaVersion  string                       `json:"policy_schema_version"`
	SourceIssue          string                       `json:"source_issue"`
	Invariants           []releaseRegressionInvariant `json:"invariants"`
	Scenarios            []releaseRegressionFixture   `json:"scenarios"`
}

type releaseRegressionInvariant struct {
	InvariantID string   `json:"invariant_id"`
	Source      string   `json:"source"`
	Scenarios   []string `json:"scenarios"`
}

type releaseRegressionFixture struct {
	ScenarioID string   `json:"scenario_id"`
	Focus      string   `json:"focus"`
	Invariants []string `json:"invariants"`
}

func TestReleaseRegressionMatrixCoversIssue714Invariants(t *testing.T) {
	matrix := loadReleaseRegressionMatrix(t)
	if matrix.FixtureSchemaVersion != releaseRegressionFixtureSchemaV1 {
		t.Fatalf("fixture schema = %q, want %q", matrix.FixtureSchemaVersion, releaseRegressionFixtureSchemaV1)
	}
	if matrix.PolicySchemaVersion != releaseRegressionPolicyVersion {
		t.Fatalf("policy schema = %q, want %q", matrix.PolicySchemaVersion, releaseRegressionPolicyVersion)
	}
	if matrix.SourceIssue != "714" {
		t.Fatalf("source issue = %q, want 714", matrix.SourceIssue)
	}
	scenarios := releaseRegressionScenarios()
	knownScenarioIDs := map[string]bool{}
	for _, fixture := range matrix.Scenarios {
		if fixture.ScenarioID == "" || fixture.Focus == "" {
			t.Fatalf("scenario fixture missing reviewable identity/focus: %#v", fixture)
		}
		if knownScenarioIDs[fixture.ScenarioID] {
			t.Fatalf("duplicate scenario fixture %s", fixture.ScenarioID)
		}
		knownScenarioIDs[fixture.ScenarioID] = true
		if _, ok := scenarios[fixture.ScenarioID]; !ok {
			t.Fatalf("matrix references scenario %s with no test scenario builder", fixture.ScenarioID)
		}
		if len(fixture.Invariants) == 0 {
			t.Fatalf("scenario %s has no invariant coverage", fixture.ScenarioID)
		}
	}
	gotInvariantIDs := map[string]bool{}
	for _, invariant := range matrix.Invariants {
		if invariant.InvariantID == "" || invariant.Source == "" {
			t.Fatalf("invariant missing reviewable identity/source: %#v", invariant)
		}
		if gotInvariantIDs[invariant.InvariantID] {
			t.Fatalf("duplicate invariant %s", invariant.InvariantID)
		}
		gotInvariantIDs[invariant.InvariantID] = true
		if len(invariant.Scenarios) == 0 {
			t.Fatalf("invariant %s lost all scenario coverage", invariant.InvariantID)
		}
		for _, scenarioID := range invariant.Scenarios {
			if !knownScenarioIDs[scenarioID] {
				t.Fatalf("invariant %s references unknown scenario %s", invariant.InvariantID, scenarioID)
			}
		}
	}
	wantInvariantIDs := append([]string(nil), issue714ReleaseInvariantIDs...)
	sort.Strings(wantInvariantIDs)
	gotSorted := keys(gotInvariantIDs)
	if !sameStringSlice(gotSorted, wantInvariantIDs) {
		t.Fatalf("issue #714 invariant set changed\ngot:  %v\nwant: %v", gotSorted, wantInvariantIDs)
	}
	for _, fixture := range matrix.Scenarios {
		for _, invariantID := range fixture.Invariants {
			if !gotInvariantIDs[invariantID] {
				t.Fatalf("scenario %s references unknown invariant %s", fixture.ScenarioID, invariantID)
			}
		}
	}
}

func TestReleaseRegressionSnapshotsStable(t *testing.T) {
	for _, scenarioID := range []string{
		"routing-churn-quota-reset-fallback",
		"handoff-crash-replay-idempotent",
		"federation-nested-bounds-concurrency",
	} {
		t.Run(scenarioID, func(t *testing.T) {
			scenario := releaseRegressionScenario(t, scenarioID)
			result, err := Execute(context.Background(), scenario, Options{})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			assertReleaseSnapshot(t, scenarioID+".canonical.json", mustCanonicalResult(t, result))
			assertReleaseSnapshot(t, scenarioID+".human.txt", []byte(HumanDecisionEvidence(result)))

			restart := scenario
			restart.StartingState = cloneDurableState(result.DurableState)
			replayed, err := Execute(context.Background(), restart, Options{ReplayJournal: &result.ReplayJournal})
			if err != nil {
				t.Fatalf("Execute replay: %v", err)
			}
			assertReleaseSnapshot(t, scenarioID+".replay.canonical.json", mustCanonicalResult(t, replayed))
		})
	}
}

func TestReleaseRegressionHighCountDeterminismAndCloseReopenReplay(t *testing.T) {
	for scenarioID := range releaseRegressionScenarios() {
		t.Run(scenarioID, func(t *testing.T) {
			scenario := releaseRegressionScenario(t, scenarioID)
			var first, firstReplay []byte
			for i := 0; i < 50; i++ {
				result, err := Execute(context.Background(), scenario, Options{})
				if err != nil {
					t.Fatalf("Execute iteration %d: %v", i, err)
				}
				current := mustCanonicalResult(t, result)
				if i == 0 {
					first = current
				} else if !bytes.Equal(first, current) {
					t.Fatalf("canonical bytes changed at iteration %d\nfirst=%s\ncurrent=%s", i, first, current)
				}

				restart := scenario
				restart.StartingState = cloneDurableState(result.DurableState)
				replayed, err := Execute(context.Background(), restart, Options{ReplayJournal: &result.ReplayJournal})
				if err != nil {
					t.Fatalf("Execute replay iteration %d: %v", i, err)
				}
				currentReplay := mustCanonicalResult(t, replayed)
				if i == 0 {
					firstReplay = currentReplay
				} else if !bytes.Equal(firstReplay, currentReplay) {
					t.Fatalf("replay canonical bytes changed at iteration %d\nfirst=%s\ncurrent=%s", i, firstReplay, currentReplay)
				}
				assertNoDuplicateReleaseSideEffects(t, replayed.DurableState)
			}
		})
	}
}

func TestReleaseRegressionHandoffCrashWindowReplayDoesNotDuplicate(t *testing.T) {
	scenario := releaseRegressionScenario(t, "handoff-crash-replay-idempotent")
	uninterrupted, err := Execute(context.Background(), scenario, Options{})
	if err != nil {
		t.Fatalf("Execute uninterrupted: %v", err)
	}
	crashed, err := Execute(context.Background(), scenario, Options{CrashAfter: 3})
	if err != nil {
		t.Fatalf("Execute crash: %v", err)
	}
	if !crashed.Truncated || crashed.TruncationReason != ErrCrashInjected {
		t.Fatalf("crash truncation = %t %q", crashed.Truncated, crashed.TruncationReason)
	}
	if got := len(crashed.DurableState.Handoffs); got != 1 {
		t.Fatalf("crashed handoffs = %d, want handoff side effect persisted before crash", got)
	}
	restart := scenario
	restart.StartingState = cloneDurableState(crashed.DurableState)
	replayed, err := Execute(context.Background(), restart, Options{ReplayJournal: &crashed.ReplayJournal})
	if err != nil {
		t.Fatalf("Execute replay: %v", err)
	}
	if got, want := mustCanonicalDurableState(t, replayed.DurableState), mustCanonicalDurableState(t, uninterrupted.DurableState); !bytes.Equal(got, want) {
		t.Fatalf("handoff crash-window replay duplicated or lost side effects\nreplay=%s\nuninterrupted=%s", got, want)
	}
	assertNoDuplicateReleaseSideEffects(t, replayed.DurableState)
}

func TestReleaseRegressionNegativeHardRequirementsAndBounds(t *testing.T) {
	t.Run("high quota cannot bypass verifier hard role", func(t *testing.T) {
		scenario := releaseRegressionBase("negative-high-quota-hard-role-bypass")
		highQuota := int64(1_000_000)
		scenario.Inventory.Models[0].Roles = []providerinventory.CatalogRole{providerinventory.CatalogRoleWorker}
		scenario.Inventory.Quotas[0].RemainingValue = &highQuota
		scenario.Inventory.Models = scenario.Inventory.Models[:1]
		scenario.Inventory.Quotas = scenario.Inventory.Quotas[:1]
		scenario.Tasks = scenario.Tasks[:1]
		scenario.Tasks[0].Role = providerinventory.CatalogRoleVerifier
		scenario.Tasks[0].CandidateModelIDs = []string{"model-a"}
		scenario.InjectedEvents = []InjectedEvent{{EventID: "event-route", Kind: "route_task", TaskID: "task-a"}}
		scenario.ConcurrencyScript = nil
		result, err := Execute(context.Background(), scenario, Options{})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if len(result.Decisions) != 1 || result.Decisions[0].Accepted {
			t.Fatalf("hard role requirement bypassed by high quota: %#v", result.Decisions)
		}
		assertDiagnosticCode(t, result.Diagnostics, ErrQuotaUnknown)
	})

	t.Run("fallback and handoff attempts are bounded", func(t *testing.T) {
		scenario := releaseRegressionScenario(t, "fallback-rate-limit-outage-bounded")
		scenario.Limits.MaxEvents = 3
		scenario.Limits.MaxSteps = 3
		result, err := Execute(context.Background(), scenario, Options{})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !result.Truncated || result.TruncationReason != "event-or-step-limit" {
			t.Fatalf("result truncation = %t %q, want event-or-step-limit", result.Truncated, result.TruncationReason)
		}
		if got := len(result.EventLog); got != 3 {
			t.Fatalf("event count = %d, want bounded to 3", got)
		}
		assertProviderFailureReceipt(t, result.DurableState.ProviderReceipts, "rate-limit")
		assertDecisionRejectedCode(t, result.Decisions, "provider-not-installed")

		handoffScenario := releaseRegressionBase("negative-bounded-handoff-attempts")
		handoffScenario.Limits.MaxEvents = 2
		handoffScenario.Limits.MaxSteps = 2
		for i := 0; i < 8; i++ {
			handoffScenario.InjectedEvents = append(handoffScenario.InjectedEvents, InjectedEvent{
				EventID:          fmt.Sprintf("event-handoff-attempt-%02d", i+1),
				Kind:             "handoff",
				TaskID:           "task-a",
				ConcurrencyGroup: "handoff-bounds",
			})
		}
		handoffScenario.ConcurrencyScript = []ConcurrencyOrder{{Group: "handoff-bounds", EventIDs: appendEventIDs(handoffScenario.InjectedEvents)}}
		handoffResult, err := Execute(context.Background(), handoffScenario, Options{})
		if err != nil {
			t.Fatalf("Execute handoff bounds: %v", err)
		}
		if !handoffResult.Truncated || handoffResult.TruncationReason != "event-or-step-limit" {
			t.Fatalf("handoff truncation = %t %q, want event-or-step-limit", handoffResult.Truncated, handoffResult.TruncationReason)
		}
		if got := len(handoffResult.DurableState.Handoffs); got != 2 {
			t.Fatalf("handoff side effects = %d, want bounded to 2", got)
		}
	})

	t.Run("cancellation before execution performs no provider work", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := Execute(ctx, releaseRegressionScenario(t, "routing-churn-quota-reset-fallback"), Options{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Execute error = %v, want context.Canceled", err)
		}
	})

	t.Run("federation depth and breadth fail closed", func(t *testing.T) {
		scenario := releaseRegressionScenario(t, "federation-nested-bounds-concurrency")
		scenario.Limits.MaxDepth = 3
		scenario.Limits.MaxBreadth = 2
		result, err := Execute(context.Background(), scenario, Options{})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		assertDiagnosticCode(t, result.Diagnostics, ErrBoundExceeded)
		if got := len(result.DurableState.AgentOwners); got != 2 {
			t.Fatalf("agent owners = %d, want bounded to 2: %#v", got, result.DurableState.AgentOwners)
		}
	})
}

func TestReleaseRegressionNegativeReplayStateAndRedaction(t *testing.T) {
	scenario := releaseRegressionScenario(t, "handoff-crash-replay-idempotent")
	applied, err := Execute(context.Background(), scenario, Options{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	t.Run("duplicate provider execution rejected", func(t *testing.T) {
		restart := scenario
		restart.StartingState = cloneDurableState(applied.DurableState)
		duplicate := restart.StartingState.ProviderReceipts[0]
		duplicate.ReceiptID = "provider_receipt_duplicate"
		restart.StartingState.ProviderReceipts = append(restart.StartingState.ProviderReceipts, duplicate)
		_, err := Execute(context.Background(), restart, Options{ReplayJournal: &applied.ReplayJournal})
		assertErrorContains(t, err, ErrInvalidFixture)
	})

	t.Run("stale ownership write rejected", func(t *testing.T) {
		restart := scenario
		restart.StartingState = cloneDurableState(applied.DurableState)
		restart.StartingState.AgentOwners[0].ResourceKey = "stale-resource"
		_, err := Execute(context.Background(), restart, Options{ReplayJournal: &applied.ReplayJournal})
		assertErrorContains(t, err, ErrInvalidFixture)
	})

	t.Run("partial durable state rejected", func(t *testing.T) {
		restart := scenario
		restart.StartingState = cloneDurableState(applied.DurableState)
		restart.StartingState.Handoffs = nil
		_, err := Execute(context.Background(), restart, Options{ReplayJournal: &applied.ReplayJournal})
		assertErrorContains(t, err, ErrInvalidFixture)
	})

	t.Run("secret canary redacted before evidence", func(t *testing.T) {
		secret := "AKIA" + strings.Repeat("B", 16)
		raw := []byte(`{"schema_version":"` + secret + `","scenario_id":"redaction-canary"}`)
		decoded, err := DecodeScenarioJSON(raw)
		if err != nil {
			t.Fatalf("DecodeScenarioJSON: %v", err)
		}
		_, err = Execute(context.Background(), decoded, Options{})
		if err == nil {
			t.Fatal("Execute accepted unsupported schema canary")
		}
		var typed *TypedError
		if !errors.As(err, &typed) {
			t.Fatalf("error = %T, want *TypedError", err)
		}
		result := Result{
			SchemaVersion: ResultSchemaV1,
			ScenarioID:    "redaction-canary",
			Diagnostics:   typed.Diagnostics,
		}
		evidence := HumanDecisionEvidence(result)
		if strings.Contains(evidence, secret) || !strings.Contains(evidence, "[REDACTED]") {
			t.Fatalf("human evidence leaked secret canary:\n%s", evidence)
		}
		canonical, err := CanonicalResultJSON(result)
		if err != nil {
			t.Fatalf("CanonicalResultJSON: %v", err)
		}
		if bytes.Contains(canonical, []byte(secret)) || !bytes.Contains(canonical, []byte("[REDACTED]")) {
			t.Fatalf("canonical evidence leaked secret canary: %s", canonical)
		}
	})
}

func releaseRegressionScenario(t *testing.T, scenarioID string) Scenario {
	t.Helper()
	builder, ok := releaseRegressionScenarios()[scenarioID]
	if !ok {
		t.Fatalf("unknown release regression scenario %s", scenarioID)
	}
	return builder()
}

func releaseRegressionScenarios() map[string]func() Scenario {
	return map[string]func() Scenario{
		"routing-churn-quota-reset-fallback":    routingChurnQuotaResetFallbackScenario,
		"routing-pin-verifier-diversity":        routingPinVerifierDiversityScenario,
		"handoff-crash-replay-idempotent":       handoffCrashReplayIdempotentScenario,
		"federation-nested-bounds-concurrency":  federationNestedBoundsConcurrencyScenario,
		"fallback-rate-limit-outage-bounded":    fallbackRateLimitOutageBoundedScenario,
		"unsupported-push-wake-truthful-follow": unsupportedPushWakeTruthfulFollowScenario,
	}
}

func routingChurnQuotaResetFallbackScenario() Scenario {
	scenario := releaseRegressionBase("routing-churn-quota-reset-fallback")
	zero := int64(0)
	unknownRemaining := int64(50)
	resetRemaining := int64(25)
	scenario.Inventory.Models[0].Availability = providerinventory.AvailabilityTemporarilyUnavailable
	scenario.Inventory.Quotas[1].Confidence = providerinventory.ConfidenceUnknown
	scenario.Inventory.Quotas[1].RemainingValue = &unknownRemaining
	scenario.Inventory.Models = append(scenario.Inventory.Models,
		releaseModel("model-c", "adapter-a", "account-a", "model-stale", providerinventory.CatalogRoleWorker),
		releaseModel("model-d", "adapter-a", "account-a", "model-exhausted", providerinventory.CatalogRoleWorker),
		releaseModel("model-zz", "adapter-a", "account-a", "model-reset", providerinventory.CatalogRoleWorker),
	)
	scenario.Inventory.Quotas = append(scenario.Inventory.Quotas,
		releaseQuota("quota-c-stale", "adapter-a", "account-a", "model-c", providerinventory.ConfidenceStale, &unknownRemaining),
		releaseQuota("quota-d-exhausted", "adapter-a", "account-a", "model-d", providerinventory.ConfidenceExact, &zero),
		releaseQuota("quota-e-reset", "adapter-a", "account-a", "model-zz", providerinventory.ConfidenceExact, &resetRemaining),
	)
	scenario.Inventory.Quotas[len(scenario.Inventory.Quotas)-1].WindowStart = "2026-07-14T01:00:00Z"
	scenario.Inventory.Quotas[len(scenario.Inventory.Quotas)-1].WindowEnd = "2026-07-15T01:00:00Z"
	scenario.Tasks[0].CandidateModelIDs = []string{"model-a", "model-b", "model-c", "model-d", "model-zz"}
	scenario.InjectedEvents = []InjectedEvent{{EventID: "event-route-reset", Kind: "route_task", TaskID: "task-a", ConcurrencyGroup: "ready"}}
	scenario.ConcurrencyScript = []ConcurrencyOrder{{Group: "ready", EventIDs: []string{"event-route-reset"}}}
	scenario.Invariants = []Invariant{{InvariantID: "inv-route-reset-fallback", Kind: "route_accepted", TaskID: "task-a"}}
	return scenario
}

func routingPinVerifierDiversityScenario() Scenario {
	scenario := releaseRegressionBase("routing-pin-verifier-diversity")
	remaining := int64(10_000)
	scenario.Inventory.Providers = append(scenario.Inventory.Providers, Provider{
		ProviderInstallationID: "provider-installation-verifier",
		AdapterID:              "adapter-verifier",
		State:                  providerinventory.InstallationInstalled,
		DiscoverySource:        providerinventory.DiscoveryFixture,
	})
	scenario.Inventory.Accounts = append(scenario.Inventory.Accounts, Account{
		AccountProfileID:       "account-verifier",
		AdapterID:              "adapter-verifier",
		ProviderInstallationID: "provider-installation-verifier",
		Readiness:              providerinventory.ReadinessReady,
	})
	scenario.Inventory.Models = append(scenario.Inventory.Models, releaseModel("model-verifier", "adapter-verifier", "account-verifier", "model-verifier", providerinventory.CatalogRoleVerifier))
	scenario.Inventory.Quotas = append(scenario.Inventory.Quotas, releaseQuota("quota-verifier", "adapter-verifier", "account-verifier", "model-verifier", providerinventory.ConfidenceExact, &remaining))
	scenario.Tasks[0].Role = providerinventory.CatalogRoleVerifier
	scenario.Tasks[0].CandidateModelIDs = []string{"model-verifier"}
	scenario.InjectedEvents = []InjectedEvent{{EventID: "event-verifier-route", Kind: "route_task", TaskID: "task-a", ConcurrencyGroup: "ready"}}
	scenario.ConcurrencyScript = []ConcurrencyOrder{{Group: "ready", EventIDs: []string{"event-verifier-route"}}}
	scenario.Invariants = []Invariant{{InvariantID: "inv-verifier-route", Kind: "route_accepted", TaskID: "task-a"}}
	return scenario
}

func handoffCrashReplayIdempotentScenario() Scenario {
	scenario := releaseRegressionBase("handoff-crash-replay-idempotent")
	scenario.InjectedEvents = []InjectedEvent{
		{EventID: "event-provider", Kind: "provider_call", TaskID: "task-a", ConcurrencyGroup: "ready"},
		{EventID: "event-budget", Kind: "budget_commit", TaskID: "task-a", ConcurrencyGroup: "ready"},
		{EventID: "event-handoff", Kind: "handoff", TaskID: "task-a", ConcurrencyGroup: "ready"},
		{EventID: "event-owner", Kind: "agent_own", TaskID: "task-a", ConcurrencyGroup: "ready"},
	}
	scenario.ConcurrencyScript = []ConcurrencyOrder{{Group: "ready", EventIDs: []string{"event-provider", "event-budget", "event-handoff", "event-owner"}}}
	scenario.Invariants = []Invariant{
		{InvariantID: "inv-handoff-route", Kind: "route_accepted", TaskID: "task-a"},
		{InvariantID: "inv-one-owner", Kind: "one_owner_per_resource"},
		{InvariantID: "inv-task-complete", Kind: "task_completed", TaskID: "task-a"},
	}
	return scenario
}

func federationNestedBoundsConcurrencyScenario() Scenario {
	scenario := releaseRegressionBase("federation-nested-bounds-concurrency")
	scenario.Limits.MaxDepth = 4
	scenario.Limits.MaxBreadth = 4
	scenario.Tasks = append(scenario.Tasks,
		releaseTask("task-c", "C", 2, "resource-c"),
		releaseTask("task-d", "D", 4, "resource-d"),
	)
	scenario.InjectedEvents = []InjectedEvent{
		{EventID: "event-own-a", Kind: "agent_own", TaskID: "task-a", ConcurrencyGroup: "federation"},
		{EventID: "event-own-b", Kind: "agent_own", TaskID: "task-b", ConcurrencyGroup: "federation"},
		{EventID: "event-own-c", Kind: "agent_own", TaskID: "task-c", ConcurrencyGroup: "federation"},
		{EventID: "event-own-d", Kind: "agent_own", TaskID: "task-d", ConcurrencyGroup: "federation"},
	}
	scenario.ConcurrencyScript = []ConcurrencyOrder{{Group: "federation", EventIDs: []string{"event-own-b", "event-own-a", "event-own-c", "event-own-d"}}}
	scenario.Invariants = []Invariant{{InvariantID: "inv-federation-one-owner", Kind: "one_owner_per_resource"}}
	return scenario
}

func fallbackRateLimitOutageBoundedScenario() Scenario {
	scenario := releaseRegressionBase("fallback-rate-limit-outage-bounded")
	scenario.Limits.MaxEvents = 12
	scenario.Limits.MaxSteps = 12
	scenario.Inventory.Providers = append(scenario.Inventory.Providers, Provider{
		ProviderInstallationID: "provider-installation-outage",
		AdapterID:              "adapter-outage",
		State:                  providerinventory.InstallationNotInstalled,
		DiscoverySource:        providerinventory.DiscoveryFixture,
	})
	scenario.Inventory.Accounts = append(scenario.Inventory.Accounts, Account{
		AccountProfileID:       "account-outage",
		AdapterID:              "adapter-outage",
		ProviderInstallationID: "provider-installation-outage",
		Readiness:              providerinventory.ReadinessReady,
	})
	remaining := int64(10)
	scenario.Inventory.Models = append(scenario.Inventory.Models, releaseModel("model-outage", "adapter-outage", "account-outage", "model-outage", providerinventory.CatalogRoleWorker))
	scenario.Inventory.Quotas = append(scenario.Inventory.Quotas, releaseQuota("quota-outage", "adapter-outage", "account-outage", "model-outage", providerinventory.ConfidenceExact, &remaining))
	scenario.Tasks[0].CandidateModelIDs = []string{"model-outage", "model-a"}
	scenario.Inventory.Failures = []ProviderFailure{{
		ModelCapabilityID: "model-a",
		FailureCode:       "rate-limit",
		AtCallOrdinal:     1,
		Retryable:         true,
		CostMicrounits:    10,
	}}
	for i := 0; i < 8; i++ {
		eventID := fmt.Sprintf("event-provider-attempt-%02d", i+1)
		scenario.InjectedEvents = append(scenario.InjectedEvents, InjectedEvent{EventID: eventID, Kind: "provider_call", TaskID: "task-a", ConcurrencyGroup: "fallback"})
		scenario.ConcurrencyScript = []ConcurrencyOrder{{Group: "fallback", EventIDs: appendEventIDs(scenario.InjectedEvents)}}
	}
	scenario.Invariants = []Invariant{{InvariantID: "inv-fallback-route", Kind: "route_accepted", TaskID: "task-a"}}
	return scenario
}

func unsupportedPushWakeTruthfulFollowScenario() Scenario {
	scenario := releaseRegressionBase("unsupported-push-wake-truthful-follow")
	scenario.Extensions["x_release_regression"] = json.RawMessage(`{"fixture_schema_version":"` + releaseRegressionFixtureSchemaV1 + `","policy_schema_version":"` + releaseRegressionPolicyVersion + `","unsupported_push_wake":"truthful-follow-poll","external_provider_calls":false}`)
	scenario.InjectedEvents = []InjectedEvent{{EventID: "event-follow-route", Kind: "route_task", TaskID: "task-a", ConcurrencyGroup: "follow"}}
	scenario.ConcurrencyScript = []ConcurrencyOrder{{Group: "follow", EventIDs: []string{"event-follow-route"}}}
	scenario.Invariants = []Invariant{{InvariantID: "inv-follow-route", Kind: "route_accepted", TaskID: "task-a"}}
	return scenario
}

func releaseRegressionBase(scenarioID string) Scenario {
	scenario := baseScenario()
	scenario.ScenarioID = scenarioID
	scenario.PolicyProvenance.PolicyVersion = releaseRegressionPolicyVersion
	scenario.PolicyProvenance.PolicyFingerprint = "sha256:" + scenarioID + "-policy"
	scenario.PolicyProvenance.PlanFingerprint = "sha256:" + scenarioID + "-plan"
	scenario.PolicyProvenance.AuthorizationFingerprint = "sha256:" + scenarioID + "-auth"
	scenario.PolicyProvenance.RoutingPolicyProfileID = "release-regression-profile"
	scenario.DurableSourceIDs.ProjectID = "project-release-regression"
	scenario.DurableSourceIDs.DeliveryRunID = "delivery-" + scenarioID
	scenario.DurableSourceIDs.InventorySourceID = "inventory-" + scenarioID
	scenario.DurableSourceIDs.BudgetSourceID = "budget-" + scenarioID
	scenario.DurableSourceIDs.RoutingSourceID = "routing-" + scenarioID
	scenario.DurableSourceIDs.AgentTreeSourceID = "agent-tree-" + scenarioID
	scenario.DurableSourceIDs.HandoffSourceID = "handoff-" + scenarioID
	scenario.DurableSourceIDs.DurableStateID = "durable-state-" + scenarioID
	scenario.Extensions = map[string]json.RawMessage{
		"x_release_regression": json.RawMessage(`{"fixture_schema_version":"` + releaseRegressionFixtureSchemaV1 + `","policy_schema_version":"` + releaseRegressionPolicyVersion + `"}`),
	}
	return scenario
}

func releaseModel(id, adapterID, accountID, canonicalID string, role providerinventory.CatalogRole) Model {
	return Model{
		ModelCapabilityID: id,
		AdapterID:         adapterID,
		AccountProfileID:  accountID,
		CanonicalModelID:  canonicalID,
		Availability:      providerinventory.AvailabilityAvailable,
		Roles:             []providerinventory.CatalogRole{role},
		CostMicrounits:    10,
	}
}

func releaseQuota(id, adapterID, accountID, modelID string, confidence providerinventory.Confidence, remaining *int64) QuotaWindow {
	return QuotaWindow{
		QuotaSnapshotID:   id,
		AdapterID:         adapterID,
		AccountProfileID:  accountID,
		ModelCapabilityID: modelID,
		QuantityKind:      providerinventory.QuantityRequests,
		WindowKind:        providerinventory.WindowFixedDay,
		Confidence:        confidence,
		RemainingValue:    remaining,
	}
}

func releaseTask(id, key string, depth int, resource string) Task {
	return Task{
		TaskID:            id,
		TaskKey:           key,
		Role:              providerinventory.CatalogRoleWorker,
		RequiredQuantity:  1,
		QuantityKind:      providerinventory.QuantityRequests,
		BudgetAuthorityID: "budget-a",
		CandidateModelIDs: []string{"model-a", "model-b"},
		AgentDepth:        depth,
		OwnerResourceKey:  resource,
	}
}

func appendEventIDs(events []InjectedEvent) []string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		out = append(out, event.EventID)
	}
	return out
}

func loadReleaseRegressionMatrix(t *testing.T) releaseRegressionMatrix {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "release_regression", "matrix.json"))
	if err != nil {
		t.Fatalf("read release regression matrix: %v", err)
	}
	var matrix releaseRegressionMatrix
	if err := json.Unmarshal(data, &matrix); err != nil {
		t.Fatalf("decode release regression matrix: %v", err)
	}
	return matrix
}

func assertReleaseSnapshot(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "release_regression", name)
	if os.Getenv(updateReleaseGoldensEnv) == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("update snapshot %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot %s: %v\nset %s=1 only for an intentional reviewed fixture/policy update", path, err, updateReleaseGoldensEnv)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("snapshot %s mismatch\nset %s=1 only for an intentional reviewed fixture/policy update\nwant=%s\ngot=%s", path, updateReleaseGoldensEnv, want, got)
	}
}

func assertNoDuplicateReleaseSideEffects(t *testing.T, state DurableState) {
	t.Helper()
	assertUniqueBy(t, "provider receipt event", state.ProviderReceipts, func(value ProviderReceipt) string {
		return value.EventID + "\x00" + value.TaskID + "\x00" + value.ModelCapabilityID
	})
	assertUniqueBy(t, "budget commitment event", state.BudgetCommitments, func(value BudgetCommitment) string {
		return value.EventID + "\x00" + value.TaskID + "\x00" + value.BudgetAuthorityID
	})
	assertUniqueBy(t, "handoff event", state.Handoffs, func(value HandoffRecord) string {
		return value.EventID + "\x00" + value.SourceTaskID + "\x00" + value.TargetTaskID
	})
	assertUniqueBy(t, "owner resource", state.AgentOwners, func(value AgentOwner) string {
		return value.ResourceKey
	})
}

func assertUniqueBy[T any](t *testing.T, name string, values []T, key func(T) string) {
	t.Helper()
	seen := map[string]bool{}
	for _, value := range values {
		current := key(value)
		if seen[current] {
			t.Fatalf("duplicate %s side effect key %q in %#v", name, current, values)
		}
		seen[current] = true
	}
}

func assertDiagnosticCode(t *testing.T, diagnostics []Diagnostic, code string) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return
		}
	}
	t.Fatalf("diagnostics = %#v, want code %s", diagnostics, code)
}

func assertProviderFailureReceipt(t *testing.T, receipts []ProviderReceipt, failureCode string) {
	t.Helper()
	for _, receipt := range receipts {
		if receipt.Status == "failed" && receipt.FailureCode == failureCode {
			return
		}
	}
	t.Fatalf("provider receipts = %#v, want failed receipt %s", receipts, failureCode)
}

func assertDecisionRejectedCode(t *testing.T, decisions []DecisionRecord, code string) {
	t.Helper()
	for _, decision := range decisions {
		for _, rejection := range decision.RejectedCandidates {
			if rejection.Code == code {
				return
			}
		}
	}
	t.Fatalf("decisions = %#v, want rejection code %s", decisions, code)
}

func assertErrorContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want containing %s", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want containing %s", err, want)
	}
}

func keys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
