package simulation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

func TestExecuteIdenticalReplayByteStable(t *testing.T) {
	scenario := baseScenario()
	first, err := Execute(context.Background(), scenario, Options{})
	if err != nil {
		t.Fatalf("Execute first: %v", err)
	}
	second, err := Execute(context.Background(), scenario, Options{})
	if err != nil {
		t.Fatalf("Execute second: %v", err)
	}
	firstJSON := mustCanonicalResult(t, first)
	secondJSON := mustCanonicalResult(t, second)
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatalf("identical scenario produced different canonical JSON\nfirst=%s\nsecond=%s", firstJSON, secondJSON)
	}

	replayScenario := scenario
	for i := range replayScenario.Inventory.Quotas {
		replayScenario.Inventory.Quotas[i].Confidence = providerinventory.ConfidenceUnknown
		replayScenario.Inventory.Quotas[i].RemainingValue = nil
	}
	replay, err := Execute(context.Background(), replayScenario, Options{ReplayJournal: &first.ReplayJournal})
	if err != nil {
		t.Fatalf("Execute replay: %v", err)
	}
	if len(replay.Diagnostics) != 0 {
		t.Fatalf("replay recomputed hidden quota truth, diagnostics = %#v", replay.Diagnostics)
	}
	if got, want := replay.Decisions[0].ChosenCandidateID, first.Decisions[0].ChosenCandidateID; got != want {
		t.Fatalf("replay chosen candidate = %q, want %q", got, want)
	}
}

func TestExecuteDifferentSeedChangesDeterministicDecision(t *testing.T) {
	first := baseScenario()
	first.Seed = 1
	second := baseScenario()
	second.Seed = 2
	gotFirst, err := Execute(context.Background(), first, Options{})
	if err != nil {
		t.Fatalf("Execute seed 1: %v", err)
	}
	gotSecond, err := Execute(context.Background(), second, Options{})
	if err != nil {
		t.Fatalf("Execute seed 2: %v", err)
	}
	if gotFirst.Decisions[0].ChosenCandidateID == gotSecond.Decisions[0].ChosenCandidateID {
		t.Fatalf("different seeds selected same candidate %q", gotFirst.Decisions[0].ChosenCandidateID)
	}
}

func TestExecuteControlledConcurrencyOrdering(t *testing.T) {
	scenario := baseScenario()
	scenario.InjectedEvents = []InjectedEvent{
		{EventID: "event-a", Kind: "route_task", TaskID: "task-a", ConcurrencyGroup: "ready"},
		{EventID: "event-b", Kind: "route_task", TaskID: "task-a", ConcurrencyGroup: "ready"},
	}
	scenario.ConcurrencyScript = []ConcurrencyOrder{{Group: "ready", EventIDs: []string{"event-b", "event-a"}}}
	result, err := Execute(context.Background(), scenario, Options{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := []string{result.EventLog[0].EventID, result.EventLog[1].EventID}; got[0] != "event-b" || got[1] != "event-a" {
		t.Fatalf("event order = %#v, want event-b then event-a", got)
	}
}

func TestExecuteRejectsReplayJournalMismatch(t *testing.T) {
	scenario := baseScenario()
	result, err := Execute(context.Background(), scenario, Options{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	result.ReplayJournal.Seed = 99
	_, err = Execute(context.Background(), scenario, Options{ReplayJournal: &result.ReplayJournal})
	if err == nil {
		t.Fatal("Execute accepted mismatched replay journal")
	}
	if !strings.Contains(err.Error(), ErrReplayMismatch) {
		t.Fatalf("error = %v, want replay mismatch", err)
	}
}

func TestExecuteReportsEventCapTruncation(t *testing.T) {
	scenario := baseScenario()
	scenario.Limits.MaxEvents = 1
	scenario.Limits.MaxSteps = 10
	result, err := Execute(context.Background(), scenario, Options{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.Truncated || result.TruncationReason != "event-or-step-limit" {
		t.Fatalf("truncation = %t %q, want event-or-step-limit", result.Truncated, result.TruncationReason)
	}
	if got := len(result.EventLog); got != 1 {
		t.Fatalf("event log length = %d, want capped to 1", got)
	}
}

func TestExecuteCrashRestartPreservesIdempotency(t *testing.T) {
	scenario := baseScenario()
	scenario.InjectedEvents = []InjectedEvent{
		{EventID: "event-call", Kind: "provider_call", TaskID: "task-a", ConcurrencyGroup: "ready"},
		{EventID: "event-budget", Kind: "budget_commit", TaskID: "task-a", ConcurrencyGroup: "ready"},
		{EventID: "event-owner", Kind: "agent_own", TaskID: "task-a", ConcurrencyGroup: "ready"},
	}
	scenario.ConcurrencyScript = []ConcurrencyOrder{{Group: "ready", EventIDs: []string{"event-call", "event-budget", "event-owner"}}}
	crashed, err := Execute(context.Background(), scenario, Options{CrashAfter: 1})
	if err != nil {
		t.Fatalf("Execute crash: %v", err)
	}
	if !crashed.Truncated || crashed.TruncationReason != ErrCrashInjected {
		t.Fatalf("crash result truncation = %t %q", crashed.Truncated, crashed.TruncationReason)
	}
	restart := scenario
	restart.StartingState = crashed.DurableState
	result, err := Execute(context.Background(), restart, Options{ReplayJournal: &crashed.ReplayJournal})
	if err != nil {
		t.Fatalf("Execute restart: %v", err)
	}
	if got := len(result.DurableState.ProviderReceipts); got != 1 {
		t.Fatalf("provider receipts = %d, want exactly one across restart", got)
	}
	if got := len(result.DurableState.BudgetCommitments); got != 1 {
		t.Fatalf("budget commitments = %d, want exactly one", got)
	}
	if got := len(result.DurableState.AgentOwners); got != 1 {
		t.Fatalf("agent owners = %d, want exactly one", got)
	}
}

func TestDecodeAndExecuteMalformedFixtureFailsClosedRedacted(t *testing.T) {
	secret := "AKIA" + strings.Repeat("A", 16)
	raw := []byte(`{"schema_version":"` + secret + `","scenario_id":"bad"}`)
	scenario, err := DecodeScenarioJSON(raw)
	if err != nil {
		t.Fatalf("DecodeScenarioJSON: %v", err)
	}
	_, err = Execute(context.Background(), scenario, Options{})
	if err == nil {
		t.Fatal("Execute returned nil error for unsupported schema")
	}
	var typed *TypedError
	if !errors.As(err, &typed) {
		t.Fatalf("error = %T, want *TypedError", err)
	}
	msg := typed.Diagnostics[0].Message
	if strings.Contains(msg, secret) || !strings.Contains(msg, "[REDACTED]") {
		t.Fatalf("diagnostic was not redacted before truncate: %q", msg)
	}

	unknown := []byte(`{"schema_version":"` + ScenarioSchemaV1 + `","scenario_id":"bad","unknown_authority":true}`)
	if _, err := DecodeScenarioJSON(unknown); err == nil {
		t.Fatal("DecodeScenarioJSON accepted unknown authority field")
	}
}

func TestExecuteInvalidCrossReferenceFailsClosed(t *testing.T) {
	scenario := baseScenario()
	scenario.Tasks[0].CandidateModelIDs = []string{"missing-model"}
	_, err := Execute(context.Background(), scenario, Options{})
	if err == nil {
		t.Fatal("Execute accepted missing candidate model")
	}
	if !strings.Contains(err.Error(), ErrMissingReference) {
		t.Fatalf("error = %v, want missing reference", err)
	}
}

func TestExecuteInvariantFailureIsTypedDiagnostic(t *testing.T) {
	scenario := baseScenario()
	scenario.InjectedEvents = []InjectedEvent{{EventID: "event-route", Kind: "route_task", TaskID: "task-a"}}
	scenario.ConcurrencyScript = nil
	scenario.Invariants = []Invariant{{InvariantID: "inv-complete", Kind: "task_completed", TaskID: "task-a"}}
	result, err := Execute(context.Background(), scenario, Options{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.InvariantResults) != 1 || result.InvariantResults[0].Passed {
		t.Fatalf("invariant results = %#v, want failure", result.InvariantResults)
	}
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != ErrInvariantFailed {
		t.Fatalf("diagnostics = %#v, want invariant failure", result.Diagnostics)
	}
}

func TestExecuteProviderNeutralInventory(t *testing.T) {
	scenario := baseScenario()
	scenario.Inventory.Providers[0].AdapterID = "neutral-provider"
	scenario.Inventory.Accounts[0].AdapterID = "neutral-provider"
	scenario.Inventory.Models[0].AdapterID = "neutral-provider"
	scenario.Inventory.Models[1].AdapterID = "neutral-provider"
	scenario.Inventory.Quotas[0].AdapterID = "neutral-provider"
	scenario.Inventory.Quotas[1].AdapterID = "neutral-provider"
	result, err := Execute(context.Background(), scenario, Options{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.Decisions[0].Accepted {
		t.Fatalf("provider-neutral route was not accepted: %#v", result.Decisions[0])
	}
}

func TestIsolatedSubprocessEnvClearsGitSelection(t *testing.T) {
	env := IsolatedSubprocessEnv([]string{
		"GIT_DIR=/repo/.git",
		"GIT_WORK_TREE=/repo",
		"GIT_INDEX_FILE=/repo/.git/index",
		"GIT_COMMON_DIR=/repo/.git",
		"PATH=/bin",
	}, "/tmp/isolated-home")
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, forbidden := range []string{"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE=", "GIT_COMMON_DIR="} {
		if strings.Contains(joined, "\n"+forbidden) {
			t.Fatalf("env retained %s: %#v", forbidden, env)
		}
	}
	for _, want := range []string{"HOME=/tmp/isolated-home", "LOOPCODER_HOME=/tmp/isolated-home", "PATH=/bin"} {
		if !strings.Contains(joined, "\n"+want+"\n") {
			t.Fatalf("env missing %s: %#v", want, env)
		}
	}
}

func TestCanonicalResultJSONStableOrdering(t *testing.T) {
	scenario := baseScenario()
	raw, err := json.Marshal(scenario)
	if err != nil {
		t.Fatalf("marshal scenario: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	payload["x_zeta"] = json.RawMessage(`{"z":1}`)
	payload["x_alpha"] = json.RawMessage(`{"a":1}`)
	raw, err = json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	decoded, err := DecodeScenarioJSON(raw)
	if err != nil {
		t.Fatalf("DecodeScenarioJSON: %v", err)
	}
	first, err := Execute(context.Background(), decoded, Options{})
	if err != nil {
		t.Fatalf("Execute first: %v", err)
	}
	second, err := Execute(context.Background(), decoded, Options{})
	if err != nil {
		t.Fatalf("Execute second: %v", err)
	}
	if !bytes.Equal(mustCanonicalResult(t, first), mustCanonicalResult(t, second)) {
		t.Fatal("canonical result JSON changed across equivalent runs")
	}
	if _, ok := first.Extensions["x_alpha"]; !ok {
		t.Fatalf("extension fields were not preserved: %#v", first.Extensions)
	}
}

func mustCanonicalResult(t *testing.T, result Result) []byte {
	t.Helper()
	data, err := CanonicalResultJSON(result)
	if err != nil {
		t.Fatalf("CanonicalResultJSON: %v", err)
	}
	return data
}

func baseScenario() Scenario {
	remaining := int64(100)
	return Scenario{
		SchemaVersion: ScenarioSchemaV1,
		ScenarioID:    "scenario-a",
		Seed:          1,
		Clock:         ClockPlan{Origin: "2026-07-14T00:00:00Z", StepMS: 10},
		Limits:        Limits{MaxEvents: 20, MaxSteps: 20, MaxDepth: 3, MaxBreadth: 4},
		DurableSourceIDs: DurableSourceIDs{
			ProjectID:         "project-a",
			DeliveryRunID:     "delivery-a",
			InventorySourceID: "inventory-fixture-a",
			BudgetSourceID:    "budget-fixture-a",
			RoutingSourceID:   "routing-fixture-a",
			AgentTreeSourceID: "agent-tree-fixture-a",
			HandoffSourceID:   "handoff-fixture-a",
			DurableStateID:    "durable-state-a",
		},
		PolicyProvenance: PolicyProvenance{
			PolicyVersion:            "simulation-policy-v1",
			PolicyFingerprint:        "sha256:policy",
			PlanFingerprint:          "sha256:plan",
			AuthorizationFingerprint: "sha256:auth",
			RoutingPolicyProfileID:   "routing-profile-a",
		},
		Inventory: Inventory{
			Providers: []Provider{{
				ProviderInstallationID: "provider-installation-a",
				AdapterID:              "adapter-a",
				State:                  providerinventory.InstallationInstalled,
				DiscoverySource:        providerinventory.DiscoveryFixture,
			}},
			Accounts: []Account{{
				AccountProfileID:       "account-a",
				AdapterID:              "adapter-a",
				ProviderInstallationID: "provider-installation-a",
				Readiness:              providerinventory.ReadinessReady,
			}},
			Models: []Model{
				{
					ModelCapabilityID: "model-a",
					AdapterID:         "adapter-a",
					AccountProfileID:  "account-a",
					CanonicalModelID:  "model-alpha",
					Availability:      providerinventory.AvailabilityAvailable,
					Roles:             []providerinventory.CatalogRole{providerinventory.CatalogRoleWorker},
					CostMicrounits:    10,
				},
				{
					ModelCapabilityID: "model-b",
					AdapterID:         "adapter-a",
					AccountProfileID:  "account-a",
					CanonicalModelID:  "model-beta",
					Availability:      providerinventory.AvailabilityAvailable,
					Roles:             []providerinventory.CatalogRole{providerinventory.CatalogRoleWorker},
					CostMicrounits:    10,
				},
			},
			Quotas: []QuotaWindow{
				{
					QuotaSnapshotID:   "quota-a",
					AdapterID:         "adapter-a",
					AccountProfileID:  "account-a",
					ModelCapabilityID: "model-a",
					QuantityKind:      providerinventory.QuantityRequests,
					WindowKind:        providerinventory.WindowFixedDay,
					Confidence:        providerinventory.ConfidenceExact,
					RemainingValue:    &remaining,
				},
				{
					QuotaSnapshotID:   "quota-b",
					AdapterID:         "adapter-a",
					AccountProfileID:  "account-a",
					ModelCapabilityID: "model-b",
					QuantityKind:      providerinventory.QuantityRequests,
					WindowKind:        providerinventory.WindowFixedDay,
					Confidence:        providerinventory.ConfidenceExact,
					RemainingValue:    &remaining,
				},
			},
			Latencies: []LatencyExpectation{
				{ModelCapabilityID: "model-a", LatencyMS: 100, SourceID: "latency-a"},
				{ModelCapabilityID: "model-b", LatencyMS: 100, SourceID: "latency-b"},
			},
		},
		BudgetAuthorities: []BudgetAuthority{{
			BudgetAuthorityID: "budget-a",
			QuantityKind:      providerinventory.QuantityRequests,
			Unit:              "request",
			LimitValue:        100,
			RemainingValue:    100,
			Confidence:        providerinventory.ConfidenceExact,
			PolicyVersion:     "budget-policy-v1",
		}},
		Tasks: []Task{{
			TaskID:              "task-a",
			TaskKey:             "A",
			Role:                providerinventory.CatalogRoleWorker,
			RequiredQuantity:    1,
			QuantityKind:        providerinventory.QuantityRequests,
			BudgetAuthorityID:   "budget-a",
			CandidateModelIDs:   []string{"model-a", "model-b"},
			HandoffTargetTaskID: "task-b",
			AgentDepth:          1,
			OwnerResourceKey:    "resource-a",
		}, {
			TaskID:            "task-b",
			TaskKey:           "B",
			Role:              providerinventory.CatalogRoleWorker,
			RequiredQuantity:  1,
			QuantityKind:      providerinventory.QuantityRequests,
			BudgetAuthorityID: "budget-a",
			CandidateModelIDs: []string{"model-a", "model-b"},
			AgentDepth:        1,
			OwnerResourceKey:  "resource-b",
		}},
		InjectedEvents: []InjectedEvent{
			{EventID: "event-route", Kind: "route_task", TaskID: "task-a", ConcurrencyGroup: "ready"},
			{EventID: "event-call", Kind: "provider_call", TaskID: "task-a", ConcurrencyGroup: "ready"},
		},
		ConcurrencyScript: []ConcurrencyOrder{{Group: "ready", EventIDs: []string{"event-route", "event-call"}}},
		Invariants:        []Invariant{{InvariantID: "inv-route", Kind: "route_accepted", TaskID: "task-a"}},
	}
}
