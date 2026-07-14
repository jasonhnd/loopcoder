package simulation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/delivery"
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

func TestExecuteCrashRestartReconstructsBudgetAndProviderOrdinals(t *testing.T) {
	scenario := restartAuthorityScenario()
	uninterrupted, err := Execute(context.Background(), scenario, Options{})
	if err != nil {
		t.Fatalf("Execute uninterrupted: %v", err)
	}
	crashed, err := Execute(context.Background(), scenario, Options{CrashAfter: 2})
	if err != nil {
		t.Fatalf("Execute crash: %v", err)
	}
	if !crashed.Truncated || crashed.TruncationReason != ErrCrashInjected {
		t.Fatalf("crash result truncation = %t %q", crashed.Truncated, crashed.TruncationReason)
	}
	restart := scenario
	restart.StartingState = crashed.DurableState
	restarted, err := Execute(context.Background(), restart, Options{ReplayJournal: &crashed.ReplayJournal})
	if err != nil {
		t.Fatalf("Execute restart: %v", err)
	}
	if got, want := mustCanonicalDurableState(t, restarted.DurableState), mustCanonicalDurableState(t, uninterrupted.DurableState); !bytes.Equal(got, want) {
		t.Fatalf("restart durable state mismatch\nrestart=%s\nuninterrupted=%s", got, want)
	}
	if got := len(restarted.DurableState.BudgetCommitments); got != 1 {
		t.Fatalf("budget commitments = %d, want exactly one after restart", got)
	}
	receipt, ok := receiptByEvent(restarted.DurableState.ProviderReceipts, "event-call-2")
	if !ok {
		t.Fatalf("missing second provider receipt: %#v", restarted.DurableState.ProviderReceipts)
	}
	if receipt.Status != "failed" || receipt.FailureCode != "ordinal-two" {
		t.Fatalf("second provider receipt = %#v, want ordinal-two failure", receipt)
	}
}

func TestExecuteRejectsMalformedStartingState(t *testing.T) {
	validCommitment := func() BudgetCommitment {
		return BudgetCommitment{
			CommitmentID:      stableID("budget_commitment", "scenario-a", "event-budget-1", "budget-a"),
			EventID:           "event-budget-1",
			TaskID:            "task-a",
			BudgetAuthorityID: "budget-a",
			QuantityKind:      string(providerinventory.QuantityRequests),
			CommittedValue:    1,
		}
	}
	validReceipt := func() ProviderReceipt {
		return ProviderReceipt{
			ReceiptID:         stableID("provider_receipt", "scenario-a", "event-call-1", "model-a"),
			EventID:           "event-call-1",
			TaskID:            "task-a",
			ModelCapabilityID: "model-a",
			Status:            "succeeded",
			LatencyMS:         100,
			CostMicrounits:    10,
		}
	}
	tests := []struct {
		name      string
		mutate    func(*Scenario)
		wantError string
	}{
		{
			name: "unknown budget authority",
			mutate: func(s *Scenario) {
				commitment := validCommitment()
				commitment.BudgetAuthorityID = "missing-budget"
				s.StartingState.AppliedEventIDs = []string{"event-budget-1"}
				s.StartingState.BudgetCommitments = []BudgetCommitment{commitment}
			},
			wantError: ErrMissingReference,
		},
		{
			name: "quantity mismatch",
			mutate: func(s *Scenario) {
				commitment := validCommitment()
				commitment.CommittedValue = 2
				s.StartingState.AppliedEventIDs = []string{"event-budget-1"}
				s.StartingState.BudgetCommitments = []BudgetCommitment{commitment}
			},
			wantError: ErrInvalidFixture,
		},
		{
			name: "duplicate semantic commitment",
			mutate: func(s *Scenario) {
				first := validCommitment()
				second := validCommitment()
				second.CommitmentID = "manual-duplicate-id"
				s.StartingState.AppliedEventIDs = []string{"event-budget-1"}
				s.StartingState.BudgetCommitments = []BudgetCommitment{first, second}
			},
			wantError: ErrInvalidFixture,
		},
		{
			name: "over limit reconstruction",
			mutate: func(s *Scenario) {
				commitment := validCommitment()
				commitment.CommittedValue = 3
				s.Tasks[0].RequiredQuantity = 3
				s.BudgetAuthorities[0].RemainingValue = 2
				s.StartingState.AppliedEventIDs = []string{"event-budget-1"}
				s.StartingState.BudgetCommitments = []BudgetCommitment{commitment}
			},
			wantError: ErrBudgetDenied,
		},
		{
			name: "duplicate semantic provider receipt",
			mutate: func(s *Scenario) {
				first := validReceipt()
				second := validReceipt()
				second.ReceiptID = "manual-duplicate-receipt"
				s.StartingState.AppliedEventIDs = []string{"event-call-1"}
				s.StartingState.CompletedTaskIDs = []string{"task-a"}
				s.StartingState.ProviderReceipts = []ProviderReceipt{first, second}
			},
			wantError: ErrInvalidFixture,
		},
		{
			name: "duplicate semantic handoff",
			mutate: func(s *Scenario) {
				s.InjectedEvents = append(s.InjectedEvents, InjectedEvent{EventID: "event-handoff", Kind: "handoff", TaskID: "task-a"})
				first := HandoffRecord{
					HandoffID:        stableID("handoff", "scenario-a", "event-handoff", "task-a", "task-b"),
					EventID:          "event-handoff",
					SourceTaskID:     "task-a",
					TargetTaskID:     "task-b",
					AuthorizationRef: "sha256:auth",
				}
				second := first
				second.HandoffID = "manual-duplicate-handoff"
				s.StartingState.AppliedEventIDs = []string{"event-handoff"}
				s.StartingState.Handoffs = []HandoffRecord{first, second}
			},
			wantError: ErrInvalidFixture,
		},
		{
			name: "duplicate semantic owner",
			mutate: func(s *Scenario) {
				s.InjectedEvents = append(s.InjectedEvents, InjectedEvent{EventID: "event-owner", Kind: "agent_own", TaskID: "task-a"})
				first := AgentOwner{
					OwnershipID:     stableID("agent_owner", "scenario-a", "resource-a"),
					EventID:         "event-owner",
					TaskID:          "task-a",
					ResourceKey:     "resource-a",
					OwnerState:      "held",
					Permission:      "orchestrate",
					SideEffectClass: "provider_launch",
				}
				second := first
				second.OwnershipID = "manual-duplicate-owner"
				s.StartingState.AppliedEventIDs = []string{"event-owner"}
				s.StartingState.AgentOwners = []AgentOwner{first, second}
			},
			wantError: ErrInvalidFixture,
		},
		{
			name: "duplicate event identity",
			mutate: func(s *Scenario) {
				s.InjectedEvents = append(s.InjectedEvents, s.InjectedEvents[0])
			},
			wantError: ErrInvalidFixture,
		},
		{
			name: "unknown completed task",
			mutate: func(s *Scenario) {
				s.StartingState.CompletedTaskIDs = []string{"missing-task"}
			},
			wantError: ErrMissingReference,
		},
		{
			name: "unknown applied event",
			mutate: func(s *Scenario) {
				s.StartingState.AppliedEventIDs = []string{"missing-event"}
			},
			wantError: ErrMissingReference,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scenario := restartAuthorityScenario()
			tt.mutate(&scenario)
			_, err := Execute(context.Background(), scenario, Options{})
			if err == nil {
				t.Fatal("Execute accepted malformed starting state")
			}
			if !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("error = %v, want %s", err, tt.wantError)
			}
		})
	}
}

func TestExecuteRejectsReplayJournalConflictingWithStartingState(t *testing.T) {
	scenario := restartAuthorityScenario()
	crashed, err := Execute(context.Background(), scenario, Options{CrashAfter: 1})
	if err != nil {
		t.Fatalf("Execute crash: %v", err)
	}
	restart := scenario
	restart.StartingState = crashed.DurableState
	conflicting := crashed.ReplayJournal
	conflicting.Events[0].ReceiptID = "invented-receipt"
	_, err = Execute(context.Background(), restart, Options{ReplayJournal: &conflicting})
	if err == nil {
		t.Fatal("Execute accepted conflicting replay journal")
	}
	if !strings.Contains(err.Error(), ErrReplayMismatch) {
		t.Fatalf("error = %v, want replay mismatch", err)
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

func mustCanonicalDurableState(t *testing.T, state DurableState) []byte {
	t.Helper()
	data, err := delivery.CanonicalJSON(normalizeDurableState(state))
	if err != nil {
		t.Fatalf("CanonicalJSON durable state: %v", err)
	}
	return data
}

func receiptByEvent(receipts []ProviderReceipt, eventID string) (ProviderReceipt, bool) {
	for _, receipt := range receipts {
		if receipt.EventID == eventID {
			return receipt, true
		}
	}
	return ProviderReceipt{}, false
}

func restartAuthorityScenario() Scenario {
	scenario := baseScenario()
	scenario.BudgetAuthorities[0].LimitValue = 2
	scenario.BudgetAuthorities[0].RemainingValue = 2
	scenario.Tasks[0].RequiredQuantity = 1
	scenario.Tasks[0].CandidateModelIDs = []string{"model-a"}
	scenario.Tasks[1].RequiredQuantity = 2
	scenario.Tasks[1].CandidateModelIDs = []string{"model-a"}
	scenario.InjectedEvents = []InjectedEvent{
		{EventID: "event-call-1", Kind: "provider_call", TaskID: "task-a", ConcurrencyGroup: "ready"},
		{EventID: "event-budget-1", Kind: "budget_commit", TaskID: "task-a", ConcurrencyGroup: "ready"},
		{EventID: "event-call-2", Kind: "provider_call", TaskID: "task-a", ConcurrencyGroup: "ready"},
		{EventID: "event-budget-2", Kind: "budget_commit", TaskID: "task-b", ConcurrencyGroup: "ready"},
	}
	scenario.ConcurrencyScript = []ConcurrencyOrder{{Group: "ready", EventIDs: []string{"event-call-1", "event-budget-1", "event-call-2", "event-budget-2"}}}
	scenario.Inventory.Failures = []ProviderFailure{{
		ModelCapabilityID: "model-a",
		FailureCode:       "ordinal-two",
		AtCallOrdinal:     2,
		CostMicrounits:    10,
	}}
	scenario.Invariants = nil
	return scenario
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
