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

	_, err = Execute(context.Background(), scenario, Options{ReplayJournal: &first.ReplayJournal})
	requireReplayMismatch(t, err)
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

func TestExecuteRejectsReplayJournalAtEmptyCrashBoundary(t *testing.T) {
	scenario := baseScenario()
	result, err := Execute(context.Background(), scenario, Options{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*ReplayJournal)
	}{
		{name: "one event", mutate: func(j *ReplayJournal) {
			j.Events = j.Events[:1]
			j.Decisions = j.Decisions[:1]
		}},
		{name: "all events", mutate: func(*ReplayJournal) {}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			journal := cloneReplayJournal(result.ReplayJournal)
			tt.mutate(&journal)
			got, err := Execute(context.Background(), scenario, Options{ReplayJournal: &journal})
			requireReplayMismatch(t, err)
			if len(got.DurableState.AppliedEventIDs) != 0 || len(got.EventLog) != 0 {
				t.Fatalf("invalid replay changed state/result: %#v", got)
			}
		})
	}
}

func TestExecuteRejectsReplayJournalEligibilitySourceMutations(t *testing.T) {
	scenario := singleProviderCallScenario()
	applied, err := Execute(context.Background(), scenario, Options{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	chosen := applied.Decisions[0].ChosenCandidateID
	tests := []struct {
		name   string
		mutate func(*Scenario)
	}{
		{name: "model unavailable", mutate: func(s *Scenario) {
			model := mustModelIndex(t, s, chosen)
			s.Inventory.Models[model].Availability = providerinventory.AvailabilityTemporarilyUnavailable
		}},
		{name: "auth not ready", mutate: func(s *Scenario) {
			model := s.Inventory.Models[mustModelIndex(t, s, chosen)]
			account := mustAccountIndex(t, s, model.AccountProfileID)
			s.Inventory.Accounts[account].Readiness = providerinventory.ReadinessNotAuthenticated
		}},
		{name: "provider not installed", mutate: func(s *Scenario) {
			model := s.Inventory.Models[mustModelIndex(t, s, chosen)]
			account := s.Inventory.Accounts[mustAccountIndex(t, s, model.AccountProfileID)]
			provider := mustProviderIndex(t, s, account.ProviderInstallationID)
			s.Inventory.Providers[provider].State = providerinventory.InstallationNotInstalled
		}},
		{name: "quota stale", mutate: func(s *Scenario) {
			quota := mustQuotaIndex(t, s, chosen)
			s.Inventory.Quotas[quota].Confidence = providerinventory.ConfidenceUnknown
		}},
		{name: "quota exhausted", mutate: func(s *Scenario) {
			exhausted := int64(0)
			quota := mustQuotaIndex(t, s, chosen)
			s.Inventory.Quotas[quota].RemainingValue = &exhausted
		}},
		{name: "budget insufficient", mutate: func(s *Scenario) {
			s.BudgetAuthorities[0].RemainingValue = 0
		}},
		{name: "role disallowed", mutate: func(s *Scenario) {
			model := mustModelIndex(t, s, chosen)
			s.Inventory.Models[model].Roles = []providerinventory.CatalogRole{providerinventory.CatalogRoleVerifier}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restart := scenario
			restart.StartingState = cloneDurableState(applied.DurableState)
			tt.mutate(&restart)
			requireReplayMismatch(t, executeReplayErr(t, restart, &applied.ReplayJournal))
		})
	}
}

func TestExecuteRejectsReplayJournalCanonicalDecisionMutations(t *testing.T) {
	t.Run("false reject first eligible", func(t *testing.T) {
		scenario := routeOnlyScenario()
		applied, err := Execute(context.Background(), scenario, Options{})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		task := scenario.Tasks[0]
		candidates := candidateModelsForScenario(scenario, task)
		if len(candidates) < 2 {
			t.Fatalf("need at least two candidates, got %#v", candidates)
		}
		rejected := map[string]bool{candidates[0].ModelCapabilityID: true}
		journal := cloneReplayJournal(applied.ReplayJournal)
		decision := &journal.Decisions[0]
		decision.ChosenCandidateID = candidates[1].ModelCapabilityID
		decision.RejectedCandidates = []Rejection{{CandidateID: candidates[0].ModelCapabilityID, Code: "model-unavailable", Reason: "caller supplied rejection"}}
		decision.QuotaSnapshotIDs = expectedReplayQuotaSnapshotIDs(scenario, scenario.InjectedEvents[0], task, *decision, candidates, rejected)
		restart := scenario
		restart.StartingState = cloneDurableState(applied.DurableState)
		requireReplayMismatch(t, executeReplayErr(t, restart, &journal))
	})

	t.Run("rejection bytes", func(t *testing.T) {
		scenario := routeOnlyScenario()
		first := candidateModelsForScenario(scenario, scenario.Tasks[0])[0].ModelCapabilityID
		scenario.Inventory.Models[mustModelIndex(t, &scenario, first)].Availability = providerinventory.AvailabilityTemporarilyUnavailable
		applied, err := Execute(context.Background(), scenario, Options{})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if len(applied.Decisions[0].RejectedCandidates) == 0 {
			t.Fatalf("expected canonical rejection: %#v", applied.Decisions[0])
		}
		tests := []struct {
			name   string
			mutate func(*ReplayJournal)
		}{
			{name: "reason", mutate: func(j *ReplayJournal) {
				j.Decisions[0].RejectedCandidates[0].Reason = "invented evidence"
			}},
			{name: "omit", mutate: func(j *ReplayJournal) {
				j.Decisions[0].RejectedCandidates = nil
			}},
			{name: "add", mutate: func(j *ReplayJournal) {
				j.Decisions[0].RejectedCandidates = append(j.Decisions[0].RejectedCandidates, Rejection{CandidateID: j.Decisions[0].ChosenCandidateID, Code: "invented", Reason: "invented"})
				sortRejections(j.Decisions[0].RejectedCandidates)
			}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				restart := scenario
				restart.StartingState = cloneDurableState(applied.DurableState)
				journal := cloneReplayJournal(applied.ReplayJournal)
				tt.mutate(&journal)
				requireReplayMismatch(t, executeReplayErr(t, restart, &journal))
			})
		}
	})
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

func TestExecuteCrashRestartEvaluatesRouteInvariantFromDurableHistory(t *testing.T) {
	scenario := routeOnlyScenario()
	scenario.Invariants = []Invariant{{InvariantID: "inv-route", Kind: "route_accepted", TaskID: "task-a"}}
	uninterrupted, err := Execute(context.Background(), scenario, Options{})
	if err != nil {
		t.Fatalf("Execute uninterrupted: %v", err)
	}
	assertRouteInvariantPassed(t, uninterrupted)

	restart := scenario
	restart.StartingState = cloneDurableState(uninterrupted.DurableState)
	restarted, err := Execute(context.Background(), restart, Options{ReplayJournal: &uninterrupted.ReplayJournal})
	if err != nil {
		t.Fatalf("Execute restart: %v", err)
	}
	assertRouteInvariantPassed(t, restarted)
	assertNoInvariantDiagnostic(t, restarted)
	if got := len(restarted.Decisions); got != 0 {
		t.Fatalf("restart decisions = %d, want no newly added decisions: %#v", got, restarted.Decisions)
	}
	if got := restarted.Diff.AddedDecisionIDs; len(got) != 0 {
		t.Fatalf("restart diff added decisions = %#v, want none", got)
	}
	if got, want := mustCanonicalInvariants(t, restarted), mustCanonicalInvariants(t, uninterrupted); !bytes.Equal(got, want) {
		t.Fatalf("restart invariant bytes mismatch\nrestart=%s\nuninterrupted=%s", got, want)
	}
}

func TestExecuteCrashRestartEvaluatesSideEffectRouteInvariantsFromDurableHistory(t *testing.T) {
	tests := []struct {
		name  string
		event InjectedEvent
	}{
		{name: "provider", event: InjectedEvent{EventID: "event-call", Kind: "provider_call", TaskID: "task-a", ConcurrencyGroup: "ready"}},
		{name: "budget", event: InjectedEvent{EventID: "event-budget", Kind: "budget_commit", TaskID: "task-a", ConcurrencyGroup: "ready"}},
		{name: "handoff", event: InjectedEvent{EventID: "event-handoff", Kind: "handoff", TaskID: "task-a", ConcurrencyGroup: "ready"}},
		{name: "ownership", event: InjectedEvent{EventID: "event-owner", Kind: "agent_own", TaskID: "task-a", ConcurrencyGroup: "ready"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scenario := baseScenario()
			scenario.InjectedEvents = []InjectedEvent{tt.event}
			scenario.ConcurrencyScript = []ConcurrencyOrder{{Group: "ready", EventIDs: []string{tt.event.EventID}}}
			scenario.Invariants = []Invariant{{InvariantID: "inv-route", Kind: "route_accepted", TaskID: "task-a"}}
			uninterrupted, err := Execute(context.Background(), scenario, Options{})
			if err != nil {
				t.Fatalf("Execute uninterrupted: %v", err)
			}
			assertRouteInvariantPassed(t, uninterrupted)

			restart := scenario
			restart.StartingState = cloneDurableState(uninterrupted.DurableState)
			restarted, err := Execute(context.Background(), restart, Options{ReplayJournal: &uninterrupted.ReplayJournal})
			if err != nil {
				t.Fatalf("Execute restart: %v", err)
			}
			assertRouteInvariantPassed(t, restarted)
			assertNoInvariantDiagnostic(t, restarted)
			if got := len(restarted.Decisions); got != 0 {
				t.Fatalf("restart decisions = %d, want no newly added decisions: %#v", got, restarted.Decisions)
			}
			if got := restarted.Diff.AddedDecisionIDs; len(got) != 0 {
				t.Fatalf("restart diff added decisions = %#v, want none", got)
			}
			if got, want := mustCanonicalInvariants(t, restarted), mustCanonicalInvariants(t, uninterrupted); !bytes.Equal(got, want) {
				t.Fatalf("restart invariant bytes mismatch\nrestart=%s\nuninterrupted=%s", got, want)
			}
		})
	}
}

func TestExecuteCrashRestartCurrentDecisionOverridesHistoricalRouteInvariant(t *testing.T) {
	scenario := baseScenario()
	scenario.BudgetAuthorities[0].RemainingValue = 1
	scenario.Tasks[0].CandidateModelIDs = []string{"model-a"}
	scenario.InjectedEvents = []InjectedEvent{
		{EventID: "event-budget-1", Kind: "budget_commit", TaskID: "task-a", ConcurrencyGroup: "ready"},
		{EventID: "event-budget-2", Kind: "budget_commit", TaskID: "task-a", ConcurrencyGroup: "ready"},
	}
	scenario.ConcurrencyScript = []ConcurrencyOrder{{Group: "ready", EventIDs: []string{"event-budget-1", "event-budget-2"}}}
	scenario.Invariants = []Invariant{{InvariantID: "inv-route", Kind: "route_accepted", TaskID: "task-a"}}
	uninterrupted, err := Execute(context.Background(), scenario, Options{})
	if err != nil {
		t.Fatalf("Execute uninterrupted: %v", err)
	}
	assertRouteInvariantFailed(t, uninterrupted)

	crashed, err := Execute(context.Background(), scenario, Options{CrashAfter: 1})
	if err != nil {
		t.Fatalf("Execute crash: %v", err)
	}
	restart := scenario
	restart.StartingState = cloneDurableState(crashed.DurableState)
	restarted, err := Execute(context.Background(), restart, Options{ReplayJournal: &crashed.ReplayJournal})
	if err != nil {
		t.Fatalf("Execute restart: %v", err)
	}
	assertRouteInvariantFailed(t, restarted)
	if got, want := mustCanonicalInvariants(t, restarted), mustCanonicalInvariants(t, uninterrupted); !bytes.Equal(got, want) {
		t.Fatalf("restart invariant bytes mismatch\nrestart=%s\nuninterrupted=%s", got, want)
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

func TestExecuteRejectsReplayJournalAuthorityFingerprintMutations(t *testing.T) {
	scenario := baseScenario()
	crashed, err := Execute(context.Background(), scenario, Options{CrashAfter: 1})
	if err != nil {
		t.Fatalf("Execute crash: %v", err)
	}
	restart := scenario
	restart.StartingState = crashed.DurableState
	tests := []struct {
		name   string
		mutate func(*DecisionRecord)
	}{
		{name: "policy", mutate: func(d *DecisionRecord) { d.PolicyFingerprint = "sha256:invented-policy" }},
		{name: "plan", mutate: func(d *DecisionRecord) { d.PlanFingerprint = "sha256:invented-plan" }},
		{name: "authorization", mutate: func(d *DecisionRecord) { d.AuthorizationFingerprint = "sha256:invented-auth" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			journal := cloneReplayJournal(crashed.ReplayJournal)
			tt.mutate(&journal.Decisions[0])
			result, err := Execute(context.Background(), restart, Options{ReplayJournal: &journal})
			requireReplayMismatch(t, err)
			if len(result.DurableState.AppliedEventIDs) != 0 {
				t.Fatalf("invalid replay produced durable result: %#v", result.DurableState)
			}
		})
	}
}

func TestExecuteRejectsReplayJournalAcceptedDecisionInvariants(t *testing.T) {
	scenario := restartAuthorityScenario()
	crashed, err := Execute(context.Background(), scenario, Options{CrashAfter: 1})
	if err != nil {
		t.Fatalf("Execute crash: %v", err)
	}
	restart := scenario
	restart.StartingState = crashed.DurableState
	tests := []struct {
		name   string
		mutate func(*ReplayJournal)
	}{
		{name: "empty chosen candidate", mutate: func(j *ReplayJournal) { j.Decisions[0].ChosenCandidateID = "" }},
		{name: "mismatched chosen candidate", mutate: func(j *ReplayJournal) { j.Decisions[0].ChosenCandidateID = "model-b" }},
		{name: "invented decision ID", mutate: func(j *ReplayJournal) {
			j.Decisions[0].DecisionID = "decision_invented"
			j.Events[0].DecisionID = "decision_invented"
		}},
		{name: "invented idempotency key", mutate: func(j *ReplayJournal) { j.Events[0].IdempotencyKey = "idem_invented" }},
		{name: "accepted rejected overlap", mutate: func(j *ReplayJournal) {
			j.Decisions[0].RejectedCandidates = append(j.Decisions[0].RejectedCandidates, Rejection{CandidateID: j.Decisions[0].ChosenCandidateID, Code: "overlap", Reason: "overlap"})
		}},
		{name: "duplicate rejected candidate", mutate: func(j *ReplayJournal) {
			j.Decisions[0].RejectedCandidates = []Rejection{
				{CandidateID: "model-a", Code: "duplicate", Reason: "duplicate"},
				{CandidateID: "model-a", Code: "duplicate", Reason: "duplicate"},
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			journal := cloneReplayJournal(crashed.ReplayJournal)
			tt.mutate(&journal)
			requireReplayMismatch(t, executeReplayErr(t, restart, &journal))
		})
	}
}

func TestExecuteRejectsReplayJournalBeyondCrashBoundary(t *testing.T) {
	tests := []struct {
		name  string
		event InjectedEvent
	}{
		{name: "commitment", event: InjectedEvent{EventID: "event-future-budget", Kind: "budget_commit", TaskID: "task-a", ConcurrencyGroup: "ready"}},
		{name: "handoff", event: InjectedEvent{EventID: "event-future-handoff", Kind: "handoff", TaskID: "task-a", ConcurrencyGroup: "ready"}},
		{name: "owner", event: InjectedEvent{EventID: "event-future-owner", Kind: "agent_own", TaskID: "task-a", ConcurrencyGroup: "ready"}},
		{name: "provider receipt", event: InjectedEvent{EventID: "event-future-call", Kind: "provider_call", TaskID: "task-a", ConcurrencyGroup: "ready"}},
		{name: "decision", event: InjectedEvent{EventID: "event-future-route", Kind: "route_task", TaskID: "task-a", ConcurrencyGroup: "ready"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scenario := baseScenario()
			scenario.InjectedEvents = []InjectedEvent{
				{EventID: "event-route", Kind: "route_task", TaskID: "task-a", ConcurrencyGroup: "ready"},
				tt.event,
			}
			scenario.ConcurrencyScript = []ConcurrencyOrder{{Group: "ready", EventIDs: []string{"event-route", tt.event.EventID}}}
			crashed, err := Execute(context.Background(), scenario, Options{CrashAfter: 1})
			if err != nil {
				t.Fatalf("Execute crash: %v", err)
			}
			uninterrupted, err := Execute(context.Background(), scenario, Options{})
			if err != nil {
				t.Fatalf("Execute uninterrupted: %v", err)
			}
			restart := scenario
			restart.StartingState = crashed.DurableState
			journal := cloneReplayJournal(crashed.ReplayJournal)
			journal.Events = append(journal.Events, uninterrupted.ReplayJournal.Events[1])
			journal.Decisions = append(journal.Decisions, uninterrupted.ReplayJournal.Decisions[1])
			requireReplayMismatch(t, executeReplayErr(t, restart, &journal))
		})
	}
}

func TestExecuteRejectsNoncanonicalDurableStableIDs(t *testing.T) {
	scenario := baseScenario()
	scenario.InjectedEvents = []InjectedEvent{
		{EventID: "event-call", Kind: "provider_call", TaskID: "task-a", ConcurrencyGroup: "ready"},
		{EventID: "event-budget", Kind: "budget_commit", TaskID: "task-a", ConcurrencyGroup: "ready"},
		{EventID: "event-handoff", Kind: "handoff", TaskID: "task-a", ConcurrencyGroup: "ready"},
		{EventID: "event-owner", Kind: "agent_own", TaskID: "task-a", ConcurrencyGroup: "ready"},
	}
	scenario.ConcurrencyScript = []ConcurrencyOrder{{Group: "ready", EventIDs: []string{"event-call", "event-budget", "event-handoff", "event-owner"}}}
	applied, err := Execute(context.Background(), scenario, Options{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*DurableState)
	}{
		{name: "provider receipt", mutate: func(s *DurableState) { s.ProviderReceipts[0].ReceiptID = "provider_receipt_alias" }},
		{name: "budget commitment", mutate: func(s *DurableState) { s.BudgetCommitments[0].CommitmentID = "budget_commitment_alias" }},
		{name: "handoff", mutate: func(s *DurableState) { s.Handoffs[0].HandoffID = "handoff_alias" }},
		{name: "ownership", mutate: func(s *DurableState) { s.AgentOwners[0].OwnershipID = "agent_owner_alias" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restart := scenario
			restart.StartingState = cloneDurableState(applied.DurableState)
			tt.mutate(&restart.StartingState)
			_, err := Execute(context.Background(), restart, Options{ReplayJournal: &applied.ReplayJournal})
			if err == nil {
				t.Fatal("Execute accepted noncanonical durable ID")
			}
			if !strings.Contains(err.Error(), ErrInvalidFixture) {
				t.Fatalf("error = %v, want invalid fixture", err)
			}
		})
	}
}

func TestExecuteRejectsReplayJournalStateEquivalenceMutations(t *testing.T) {
	scenario := baseScenario()
	scenario.InjectedEvents = []InjectedEvent{
		{EventID: "event-call", Kind: "provider_call", TaskID: "task-a", ConcurrencyGroup: "ready"},
		{EventID: "event-budget", Kind: "budget_commit", TaskID: "task-a", ConcurrencyGroup: "ready"},
		{EventID: "event-handoff", Kind: "handoff", TaskID: "task-a", ConcurrencyGroup: "ready"},
		{EventID: "event-owner", Kind: "agent_own", TaskID: "task-a", ConcurrencyGroup: "ready"},
	}
	scenario.ConcurrencyScript = []ConcurrencyOrder{{Group: "ready", EventIDs: []string{"event-call", "event-budget", "event-handoff", "event-owner"}}}
	applied, err := Execute(context.Background(), scenario, Options{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	tests := []struct {
		name    string
		mutate  func(*Scenario, *ReplayJournal)
		wantErr string
	}{
		{name: "wrong applied status", mutate: func(_ *Scenario, j *ReplayJournal) {
			j.Events[1].Status = "failed"
		}, wantErr: ErrReplayMismatch},
		{name: "wrong generation", mutate: func(_ *Scenario, j *ReplayJournal) {
			j.Events[1].Sequence = 99
		}, wantErr: ErrReplayMismatch},
		{name: "wrong commitment quantity", mutate: func(s *Scenario, _ *ReplayJournal) {
			s.StartingState.BudgetCommitments[0].CommittedValue++
		}, wantErr: ErrInvalidFixture},
		{name: "wrong handoff authority", mutate: func(s *Scenario, _ *ReplayJournal) {
			s.StartingState.Handoffs[0].AuthorizationRef = "sha256:invented-auth"
		}, wantErr: ErrInvalidFixture},
		{name: "wrong owner scope", mutate: func(s *Scenario, _ *ReplayJournal) {
			s.StartingState.AgentOwners[0].ResourceKey = "invented-resource"
		}, wantErr: ErrInvalidFixture},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restart := scenario
			restart.StartingState = cloneDurableState(applied.DurableState)
			journal := cloneReplayJournal(applied.ReplayJournal)
			tt.mutate(&restart, &journal)
			_, err := Execute(context.Background(), restart, Options{ReplayJournal: &journal})
			if err == nil {
				t.Fatal("Execute accepted mismatched replay/state")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %s", err, tt.wantErr)
			}
		})
	}
}

func TestExecuteRejectsAppliedEventsWithoutMaterializedDurableRecords(t *testing.T) {
	scenario := allSideEffectScenario()
	applied, err := Execute(context.Background(), scenario, Options{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	tests := []struct {
		name    string
		mutate  func(*DurableState)
		wantErr string
	}{
		{name: "provider receipt removed", mutate: func(s *DurableState) {
			s.ProviderReceipts = nil
		}, wantErr: ErrInvalidFixture},
		{name: "budget commitment removed", mutate: func(s *DurableState) {
			s.BudgetCommitments = nil
		}, wantErr: ErrInvalidFixture},
		{name: "handoff removed", mutate: func(s *DurableState) {
			s.Handoffs = nil
		}, wantErr: ErrInvalidFixture},
		{name: "owner removed", mutate: func(s *DurableState) {
			s.AgentOwners = nil
		}, wantErr: ErrInvalidFixture},
		{name: "provider receipt mismatched", mutate: func(s *DurableState) {
			s.ProviderReceipts[0].LatencyMS++
		}, wantErr: ErrInvalidFixture},
		{name: "extra applied route", mutate: func(s *DurableState) {
			s.AppliedEventIDs = append(s.AppliedEventIDs, "event-extra-route")
		}, wantErr: ErrReplayMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restart := scenario
			restart.InjectedEvents = append(restart.InjectedEvents, InjectedEvent{EventID: "event-extra-route", Kind: "route_task", TaskID: "task-a", ConcurrencyGroup: "ready"})
			restart.StartingState = cloneDurableState(applied.DurableState)
			tt.mutate(&restart.StartingState)
			_, err := Execute(context.Background(), restart, Options{ReplayJournal: &applied.ReplayJournal})
			if err == nil {
				t.Fatal("Execute accepted non-equivalent starting state")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %s", err, tt.wantErr)
			}
		})
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

func mustCanonicalInvariants(t *testing.T, results Result) []byte {
	t.Helper()
	data, err := delivery.CanonicalJSON(results.InvariantResults)
	if err != nil {
		t.Fatalf("CanonicalJSON invariants: %v", err)
	}
	return data
}

func assertRouteInvariantPassed(t *testing.T, result Result) {
	t.Helper()
	if len(result.InvariantResults) != 1 || !result.InvariantResults[0].Passed {
		t.Fatalf("invariant results = %#v, want one passed route invariant; diagnostics=%#v", result.InvariantResults, result.Diagnostics)
	}
}

func assertRouteInvariantFailed(t *testing.T, result Result) {
	t.Helper()
	if len(result.InvariantResults) != 1 || result.InvariantResults[0].Passed {
		t.Fatalf("invariant results = %#v, want one failed route invariant", result.InvariantResults)
	}
	found := false
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Code == ErrInvariantFailed {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("diagnostics = %#v, want %s", result.Diagnostics, ErrInvariantFailed)
	}
}

func assertNoInvariantDiagnostic(t *testing.T, result Result) {
	t.Helper()
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Code == ErrInvariantFailed {
			t.Fatalf("diagnostics = %#v, want no %s", result.Diagnostics, ErrInvariantFailed)
		}
	}
}

func receiptByEvent(receipts []ProviderReceipt, eventID string) (ProviderReceipt, bool) {
	for _, receipt := range receipts {
		if receipt.EventID == eventID {
			return receipt, true
		}
	}
	return ProviderReceipt{}, false
}

func executeReplayErr(t *testing.T, scenario Scenario, journal *ReplayJournal) error {
	t.Helper()
	result, err := Execute(context.Background(), scenario, Options{ReplayJournal: journal})
	if err == nil && len(result.Diagnostics) != 0 {
		t.Fatalf("Execute returned diagnostics instead of failing closed: %#v", result.Diagnostics)
	}
	return err
}

func cloneReplayJournal(journal ReplayJournal) ReplayJournal {
	journal.Events = append([]EventRecord(nil), journal.Events...)
	journal.Decisions = append([]DecisionRecord(nil), journal.Decisions...)
	for i := range journal.Decisions {
		journal.Decisions[i].RejectedCandidates = append([]Rejection(nil), journal.Decisions[i].RejectedCandidates...)
		journal.Decisions[i].QuotaSnapshotIDs = append([]string(nil), journal.Decisions[i].QuotaSnapshotIDs...)
	}
	return journal
}

func cloneDurableState(state DurableState) DurableState {
	state.AppliedEventIDs = append([]string(nil), state.AppliedEventIDs...)
	state.ProviderReceipts = append([]ProviderReceipt(nil), state.ProviderReceipts...)
	state.HostDeliveries = append([]HostDelivery(nil), state.HostDeliveries...)
	state.PartialResults = append([]PartialResult(nil), state.PartialResults...)
	state.OwnerConflicts = append([]OwnerConflict(nil), state.OwnerConflicts...)
	state.BudgetCommitments = append([]BudgetCommitment(nil), state.BudgetCommitments...)
	state.Handoffs = append([]HandoffRecord(nil), state.Handoffs...)
	state.AgentOwners = append([]AgentOwner(nil), state.AgentOwners...)
	state.CompletedTaskIDs = append([]string(nil), state.CompletedTaskIDs...)
	return state
}

func requireReplayMismatch(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("Execute accepted invalid replay journal")
	}
	if !strings.Contains(err.Error(), ErrReplayMismatch) {
		t.Fatalf("error = %v, want replay mismatch", err)
	}
}

func routeOnlyScenario() Scenario {
	scenario := baseScenario()
	scenario.InjectedEvents = []InjectedEvent{{EventID: "event-route", Kind: "route_task", TaskID: "task-a", ConcurrencyGroup: "ready"}}
	scenario.ConcurrencyScript = []ConcurrencyOrder{{Group: "ready", EventIDs: []string{"event-route"}}}
	scenario.Invariants = nil
	return scenario
}

func singleProviderCallScenario() Scenario {
	scenario := baseScenario()
	scenario.InjectedEvents = []InjectedEvent{{EventID: "event-call", Kind: "provider_call", TaskID: "task-a", ConcurrencyGroup: "ready"}}
	scenario.ConcurrencyScript = []ConcurrencyOrder{{Group: "ready", EventIDs: []string{"event-call"}}}
	scenario.Invariants = nil
	return scenario
}

func allSideEffectScenario() Scenario {
	scenario := baseScenario()
	scenario.InjectedEvents = []InjectedEvent{
		{EventID: "event-call", Kind: "provider_call", TaskID: "task-a", ConcurrencyGroup: "ready"},
		{EventID: "event-budget", Kind: "budget_commit", TaskID: "task-a", ConcurrencyGroup: "ready"},
		{EventID: "event-handoff", Kind: "handoff", TaskID: "task-a", ConcurrencyGroup: "ready"},
		{EventID: "event-owner", Kind: "agent_own", TaskID: "task-a", ConcurrencyGroup: "ready"},
	}
	scenario.ConcurrencyScript = []ConcurrencyOrder{{Group: "ready", EventIDs: []string{"event-call", "event-budget", "event-handoff", "event-owner"}}}
	scenario.Invariants = nil
	return scenario
}

func mustModelIndex(t *testing.T, scenario *Scenario, modelID string) int {
	t.Helper()
	for i, model := range scenario.Inventory.Models {
		if model.ModelCapabilityID == modelID {
			return i
		}
	}
	t.Fatalf("missing model %s", modelID)
	return 0
}

func mustAccountIndex(t *testing.T, scenario *Scenario, accountID string) int {
	t.Helper()
	for i, account := range scenario.Inventory.Accounts {
		if account.AccountProfileID == accountID {
			return i
		}
	}
	t.Fatalf("missing account %s", accountID)
	return 0
}

func mustProviderIndex(t *testing.T, scenario *Scenario, providerID string) int {
	t.Helper()
	for i, provider := range scenario.Inventory.Providers {
		if provider.ProviderInstallationID == providerID {
			return i
		}
	}
	t.Fatalf("missing provider %s", providerID)
	return 0
}

func mustQuotaIndex(t *testing.T, scenario *Scenario, modelID string) int {
	t.Helper()
	for i, quota := range scenario.Inventory.Quotas {
		if quota.ModelCapabilityID == modelID {
			return i
		}
	}
	t.Fatalf("missing quota for model %s", modelID)
	return 0
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
		Host: HostRuntime{
			ConductorHostID: "host-fixture-a",
			AdapterID:       "host-local",
			Capabilities: HostCapabilities{
				SupportsFollow: true,
				SupportsPoll:   true,
			},
		},
		Limits: Limits{MaxEvents: 20, MaxSteps: 20, MaxDepth: 3, MaxBreadth: 4},
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
