package routing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/budget"
	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

func TestResumeApprovedHandoffRoutesProviderAToProviderBOnce(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	for i := range input.Inputs.Inventory.QuotaSnapshots {
		if input.Inputs.Inventory.QuotaSnapshots[i].AdapterID == "codex" {
			remaining := int64(0)
			input.Inputs.Inventory.QuotaSnapshots[i].RemainingValue = &remaining
		}
		if input.Inputs.Inventory.QuotaSnapshots[i].AdapterID == "claude" {
			input.Inputs.Inventory.QuotaSnapshots[i].Confidence = providerinventory.ConfidenceExact
		}
	}

	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:           handoff.HandoffID,
		DecisionInput:       input,
		ReservationValue:    1,
		ReusableEvidenceIDs: []string{"qsnap-codex-a-good", "availability-observation-a"},
		DecidedBy:           routerActor(),
		Host:                routingHost(),
	})
	if err != nil {
		t.Fatalf("ResumeApprovedHandoff: %v\n%s", err, ExplainHuman(result.RoutingDecision))
	}
	if result.Successor.AttemptID == "" || result.Successor.SourceAttemptID != handoff.SourceAttemptID || result.Successor.HandoffGeneration != handoff.HandoffGeneration {
		t.Fatalf("successor launch = %#v, want linked successor", result.Successor)
	}
	if got := selectedCandidate(result.RoutingDecision).AdapterID; got != "claude" {
		t.Fatalf("selected adapter = %q, want provider B claude", got)
	}
	if result.Handoff.AcceptedTaskFingerprint != handoff.AcceptedTaskFingerprint || result.Handoff.AuthorizationFingerprint != handoff.AuthorizationFingerprint {
		t.Fatalf("handoff identity changed: %#v vs %#v", result.Handoff, handoff)
	}
	registration, err := storage.ValidateNativeChildLaunch(ctx, store, handoff.ChildRunID, handoff.DestinationExecutorID, handoff.HandoffGeneration)
	if err != nil {
		t.Fatalf("ValidateNativeChildLaunch: %v", err)
	}
	if registration.AdapterID != "claude" || registration.ProviderSessionRef != "" || registration.AttemptID != result.Successor.AttemptID {
		t.Fatalf("registration after successor = %#v", registration)
	}

	replayed, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:           handoff.HandoffID,
		DecisionInput:       input,
		ReservationValue:    1,
		ReusableEvidenceIDs: []string{"qsnap-codex-a-good", "availability-observation-a"},
		DecidedBy:           routerActor(),
		Host:                routingHost(),
	})
	if err != nil {
		t.Fatalf("replay ResumeApprovedHandoff: %v", err)
	}
	if replayed.Successor.AttemptID != result.Successor.AttemptID || countSuccessorAttempts(t, ctx, store, handoff.TaskID) != 1 {
		t.Fatalf("replay created duplicate successor: %#v first=%#v count=%d", replayed.Successor, result.Successor, countSuccessorAttempts(t, ctx, store, handoff.TaskID))
	}
}

func TestResumeApprovedHandoffConcurrentReplayLaunchesOnce(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	path := tempDB(t)
	store, handoff, input := handoffResumeFixtureAtPath(t, ctx, fixture, path)
	defer store.Close()
	for i := range input.Inputs.Inventory.QuotaSnapshots {
		if input.Inputs.Inventory.QuotaSnapshots[i].AdapterID == "codex" {
			remaining := int64(0)
			input.Inputs.Inventory.QuotaSnapshots[i].RemainingValue = &remaining
		}
		if input.Inputs.Inventory.QuotaSnapshots[i].AdapterID == "claude" {
			input.Inputs.Inventory.QuotaSnapshots[i].Confidence = providerinventory.ConfidenceExact
		}
	}
	const workers = 6
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan HandoffResumeResult, workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := store
			if i%2 == 1 {
				other, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return fixture.now }})
				if err != nil {
					errs <- err
					return
				}
				defer other.Close()
				target = other
			}
			<-start
			result, err := ResumeApprovedHandoff(ctx, target, HandoffResumeInput{
				HandoffID:        handoff.HandoffID,
				DecisionInput:    input,
				ReservationValue: 1,
				DecidedBy:        routerActor(),
				Host:             routingHost(),
			})
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent resume error: %v", err)
	}
	var first string
	for result := range results {
		if first == "" {
			first = result.Successor.AttemptID
		}
		if result.Successor.AttemptID != first {
			t.Fatalf("concurrent successor attempt = %s, want %s", result.Successor.AttemptID, first)
		}
	}
	if countSuccessorAttempts(t, ctx, store, handoff.TaskID) != 1 {
		t.Fatalf("successor attempts = %d, want 1", countSuccessorAttempts(t, ctx, store, handoff.TaskID))
	}
}

func TestResumeApprovedHandoffNoEligiblePersistsBlockedDecision(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	for i := range input.Inputs.Inventory.QuotaSnapshots {
		remaining := int64(0)
		input.Inputs.Inventory.QuotaSnapshots[i].RemainingValue = &remaining
	}
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		DecidedBy:        routerActor(),
		Host:             routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrNoEligibleCandidate) {
		t.Fatalf("ResumeApprovedHandoff error = %v, want ErrNoEligibleCandidate", err)
	}
	if !result.Blocked || result.RoutingDecision.DecisionStatus != DecisionStatusNoEligible || len(result.RoutingDecision.RejectedSummary) == 0 {
		t.Fatalf("blocked result = %#v, want no-eligible decision with rejected reasons", result.RoutingDecision)
	}
	if countSuccessorAttempts(t, ctx, store, handoff.TaskID) != 0 {
		t.Fatalf("blocked route launched successor")
	}
}

func TestResumeApprovedHandoffCancellationAfterReservationReleasesBeforeLaunch(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	for i := range input.Inputs.Inventory.QuotaSnapshots {
		if input.Inputs.Inventory.QuotaSnapshots[i].AdapterID == "codex" {
			remaining := int64(0)
			input.Inputs.Inventory.QuotaSnapshots[i].RemainingValue = &remaining
		}
		if input.Inputs.Inventory.QuotaSnapshots[i].AdapterID == "claude" {
			input.Inputs.Inventory.QuotaSnapshots[i].Confidence = providerinventory.ConfidenceExact
		}
	}
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		Cancelled:        true,
		DecidedBy:        routerActor(),
		Host:             routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("cancelled resume error = %v, want ErrReplanRequired", err)
	}
	if result.Reservation.BudgetReservationID == "" {
		t.Fatalf("test did not reserve before cancellation")
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateReleased) {
		t.Fatalf("reservation state = %q, want released", state)
	}
	if countSuccessorAttempts(t, ctx, store, handoff.TaskID) != 0 {
		t.Fatalf("cancelled resume launched successor")
	}
}

func TestResumeApprovedHandoffStaleGenerationFencePreventsLaunch(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE run_claims SET claim_generation = claim_generation + 1 WHERE run_id = ?`, handoff.ChildRunID)
		return err
	}); err != nil {
		t.Fatalf("stale claim: %v", err)
	}
	_, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		DecidedBy:        routerActor(),
		Host:             routingHost(),
	})
	if !errors.Is(err, storage.ErrStaleClaim) {
		t.Fatalf("stale generation error = %v, want ErrStaleClaim", err)
	}
	if countSuccessorAttempts(t, ctx, store, handoff.TaskID) != 0 {
		t.Fatalf("stale generation launched successor")
	}
}

func TestResumeApprovedHandoffRejectsUnsafeArtifactRefAndReleasesReservation(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:           handoff.HandoffID,
		DecisionInput:       input,
		ReservationValue:    1,
		ReusableEvidenceIDs: []string{"provider-session-token"},
		DecidedBy:           routerActor(),
		Host:                routingHost(),
	})
	if err == nil {
		t.Fatalf("unsafe artifact ref unexpectedly succeeded")
	}
	if result.Reservation.BudgetReservationID == "" {
		t.Fatalf("test did not reserve before artifact validation")
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateReleased) {
		t.Fatalf("unsafe artifact reservation state = %q, want released", state)
	}
	if countSuccessorAttempts(t, ctx, store, handoff.TaskID) != 0 {
		t.Fatalf("unsafe artifact launched successor")
	}
}

func TestResumeApprovedHandoffReplayAfterPreLaunchStopReusesRouteAndLaunches(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	path := tempDB(t)
	store, handoff, input := handoffResumeFixtureAtPath(t, ctx, fixture, path)
	makeClaudeEligibleOnly(input)
	stopErr := errors.New("stop after reservation")
	first, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		BeforeLaunch:     func() error { return stopErr },
		DecidedBy:        routerActor(),
		Host:             routingHost(),
	})
	if !errors.Is(err, stopErr) {
		t.Fatalf("pre-launch stop error = %v, want %v", err, stopErr)
	}
	if first.Reservation.BudgetReservationID == "" || reservationState(t, ctx, store, first.Reservation.BudgetReservationID) != string(budget.StateReleased) {
		t.Fatalf("pre-launch stop did not release reservation: %#v", first.Reservation)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	reopened, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	replayed, err := ResumeApprovedHandoff(ctx, reopened, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		DecidedBy:        routerActor(),
		Host:             routingHost(),
	})
	if err != nil {
		t.Fatalf("replay after restart: %v", err)
	}
	if replayed.RoutingDecision.RoutingDecisionID != first.RoutingDecision.RoutingDecisionID || replayed.Successor.AttemptID == "" {
		t.Fatalf("replay result = %#v, first route %s", replayed, first.RoutingDecision.RoutingDecisionID)
	}
}

func TestResumeApprovedHandoffDestinationFailureUsesBoundedFallback(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	for i := range input.Inputs.Inventory.QuotaSnapshots {
		if input.Inputs.Inventory.QuotaSnapshots[i].AdapterID == "codex" {
			remaining := int64(0)
			input.Inputs.Inventory.QuotaSnapshots[i].RemainingValue = &remaining
		}
		if input.Inputs.Inventory.QuotaSnapshots[i].AdapterID == "claude" {
			input.Inputs.Inventory.QuotaSnapshots[i].Confidence = providerinventory.ConfidenceExact
		}
	}
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		DecidedBy:        routerActor(),
		Host:             routingHost(),
	})
	if err != nil {
		t.Fatalf("ResumeApprovedHandoff: %v", err)
	}
	fallback, err := HandoffDestinationFailureFallback(ctx, store, result, input.Inputs, FallbackTriggerWorkerFailed, schedulerActor(), routingHost())
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("destination failure fallback error = %v, want bounded ErrReplanRequired; fallback=%#v", err, fallback)
	}
	if fallback.DecisionStatus != FallbackStatusReplanRequired || fallback.BoundsRemaining.MaxFallbacks == 0 || len(fallback.AttemptLineage) != 2 {
		t.Fatalf("fallback = %#v, want bounded policy-preserving fallback with lineage", fallback)
	}
}

func makeClaudeEligibleOnly(input DecisionInput) {
	for i := range input.Inputs.Inventory.QuotaSnapshots {
		if input.Inputs.Inventory.QuotaSnapshots[i].AdapterID == "codex" {
			remaining := int64(0)
			input.Inputs.Inventory.QuotaSnapshots[i].RemainingValue = &remaining
		}
		if input.Inputs.Inventory.QuotaSnapshots[i].AdapterID == "claude" {
			input.Inputs.Inventory.QuotaSnapshots[i].Confidence = providerinventory.ConfidenceExact
		}
	}
}

func handoffResumeFixture(t *testing.T, ctx context.Context, fixture hardFixture) (storage.Store, storage.HandoffTransaction, DecisionInput) {
	t.Helper()
	return handoffResumeFixtureAtPath(t, ctx, fixture, tempDB(t))
}

func handoffResumeFixtureAtPath(t *testing.T, ctx context.Context, fixture hardFixture, path string) (storage.Store, storage.HandoffTransaction, DecisionInput) {
	t.Helper()
	store, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	seedRoutingDecisionStore(t, ctx, store, fixture.now)
	input := replayDecisionInput(fixture)
	input.Inputs.Requirement.PermissionRequired = taskrequirements.PermissionReadOnly
	input.Inputs.Requirement.SideEffectClass = taskrequirements.SideEffectLocalRead
	input.Inputs.Requirement.RoleKey = RoleKeyLuna
	input.Inputs.Requirement.ScopeJSON = `{"paths":["README.md"],"repo":"proj-routing"}`
	for i := range input.Inputs.Candidates {
		input.Inputs.Candidates[i].Permission = taskrequirements.PermissionReadOnly
		input.Inputs.Candidates[i].LaunchSideEffectClass = taskrequirements.SideEffectLocalRead
		input.Inputs.Candidates[i].RoleKey = RoleKeyLuna
		input.Inputs.Candidates[i].RoutingCandidateID = ""
		input.Inputs.Candidates[i].CandidateFingerprint = ""
	}
	input.Inputs.Requirement = persistFallbackTaskRequirement(t, ctx, store, input.Inputs.Requirement, fixture.now)
	input.TaskRequirementID = input.Inputs.Requirement.TaskRequirementID
	profile := BalancedRoutingPolicyProfile(fixture.now)
	input.RoutingPolicyProfile = profile
	input.RoutingPolicyProfileID = profile.RoutingPolicyProfileID
	input.PolicyFingerprint = profile.PolicyFingerprint
	input.Inputs.Policy = profile.EligibilityPolicy
	if _, err := PersistRoutingPolicyProfile(ctx, store, profile); err != nil {
		t.Fatalf("PersistRoutingPolicyProfile: %v", err)
	}
	seedHandoffRuntime(t, ctx, store, fixture, input)
	claim, err := storage.ClaimChildRunExecution(ctx, store, "run-routing", "run-routing-child", "executor-source", fixture.now, fixture.now.Add(time.Hour))
	if err != nil {
		t.Fatalf("ClaimChildRunExecution: %v", err)
	}
	seedHandoffAgentAuthority(t, ctx, store, fixture, claim)
	handoff, err := storage.RecordHandoffTransaction(ctx, store, storage.HandoffRequest{
		IdempotencyKey:            "handoff-quota-a",
		ProjectID:                 "proj-routing",
		DeliveryRunID:             "drun-routing",
		TaskID:                    "task-a",
		ParentRunID:               "run-routing",
		ChildRunID:                "run-routing-child",
		SourceAttemptID:           "attempt-source",
		SourceExecutorID:          claim.ExecutorID,
		SourceClaimGeneration:     claim.ClaimGeneration,
		TriggerKind:               storage.HandoffTriggerQuotaAvailability,
		ReasonCodes:               []string{"quota-exhausted"},
		EvidenceRecordIDs:         []string{"qsnap-codex-a-good"},
		TriggerSnapshotJSON:       `{"reason_codes":["quota-exhausted"],"remaining":0}`,
		SideEffectState:           storage.SideEffectStateReadOnly,
		DestinationExecutorID:     "handoff-destination",
		RequestedAt:               fixture.now.Add(time.Minute).Format(time.RFC3339Nano),
		DestinationLeaseExpiresAt: fixture.now.Add(time.Hour).Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("RecordHandoffTransaction: %v", err)
	}
	return store, handoff, input
}

func seedHandoffRuntime(t *testing.T, ctx context.Context, store storage.Store, fixture hardFixture, input DecisionInput) {
	t.Helper()
	at := delivery.CanonicalTimestamp(fixture.now)
	if err := storage.PersistChildPlanGraph(ctx, store,
		storage.RunNode{RunID: "run-routing", ProjectID: "proj-routing", RootRunID: "run-routing", Depth: 0, Origin: "test", Status: "running", CreatedAt: at, UpdatedAt: at},
		[]storage.RunNode{{RunID: "run-routing-child", ProjectID: "proj-routing", ParentRunID: "run-routing", RootRunID: "run-routing", Depth: 1, Origin: "handoff", Status: "planned", CreatedAt: at, UpdatedAt: at}},
		storage.ChildPlanRecord{PlanID: "plan-child", ParentRunID: "run-routing", RootRunID: "run-routing", SchemaVersion: "loopcoder.child_plan.v1", MaxDepth: 2, MaxConcurrency: 1, PlanJSON: "{}", CreatedAt: at},
		[]storage.RunEdgeRecord{{ParentRunID: "run-routing", ChildRunID: "run-routing-child", RootRunID: "run-routing", PlanID: "plan-child", ChildKey: "worker", Depth: 1, Ordinal: 0, ScopeJSON: "{}", Permission: storage.PermissionReadOnly, AggregationJSON: "{}", Status: "planned", CreatedAt: at, UpdatedAt: at}},
	); err != nil {
		t.Fatalf("PersistChildPlanGraph: %v", err)
	}
	seedProviderInventoryRows(t, ctx, store, fixture)
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO delivery_attempts(
			attempt_id, schema_version, record_version, project_id, delivery_run_id, task_id, attempt_ordinal, state,
			claim_generation, executor_id, provider_idempotency_key, side_effect_class, started_at, created_at, updated_at,
			created_by_json, updated_by_json, host_json)
			VALUES ('attempt-source', 'loopcoder.attempt.v1', 1, 'proj-routing', 'drun-routing', 'task-a', 1, 'running',
			1, 'executor-source', 'provider-source-key', ?, ?, ?, ?, '{}', '{}', '{}')`,
			taskrequirements.SideEffectLocalRead, at, at, at)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE delivery_tasks SET state = 'running', permission = ?, side_effect_class = ?, scope_json = ?, active_attempt_id = 'attempt-source', attempt_count = 1 WHERE task_id = 'task-a'`,
			taskrequirements.PermissionReadOnly, taskrequirements.SideEffectLocalRead, `{"paths":["README.md"],"repo":"proj-routing"}`)
		return err
	}); err != nil {
		t.Fatalf("seed source attempt: %v", err)
	}
	for _, c := range input.Inputs.Candidates {
		scope := budget.Scope{ScopeKind: budget.ScopeSubAgent, ProjectID: "proj-routing", DeliveryRunID: "drun-routing", TaskID: "task-a", SubAgentID: "agent-source", AdapterID: c.AdapterID, AccountProfileID: c.AccountProfileID, ModelCapabilityID: c.ModelCapabilityID}
		if _, err := budget.UpsertPolicy(ctx, store, budget.PolicyInput{
			Scope:         scope,
			QuantityKind:  providerinventory.QuantityLocalPolicy,
			WindowKind:    providerinventory.WindowUnbounded,
			PolicyMode:    budget.PolicyHard,
			CeilingValue:  100,
			PolicyVersion: "handoff-test",
			Ordinal:       c.AdapterID,
			Actor:         budget.Actor{ActorID: "test"},
			Host:          budget.Host{HostID: "test"},
		}); err != nil {
			t.Fatalf("upsert destination budget policy: %v", err)
		}
	}
}

func seedProviderInventoryRows(t *testing.T, ctx context.Context, store storage.Store, fixture hardFixture) {
	t.Helper()
	at := delivery.CanonicalTimestamp(fixture.now)
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		for _, adapter := range []string{"codex", "claude"} {
			if _, err := tx.Exec(ctx, `INSERT INTO adapter_declarations(
				adapter_declaration_id, schema_version, record_version, adapter_id, adapter_version, display_name,
				executable_names_json, created_at, updated_at, payload_json)
				VALUES (?, 'loopcoder.adapter_declaration.v1', 1, ?, 'test', ?, '[]', ?, ?, '{}')
				ON CONFLICT(adapter_declaration_id) DO NOTHING`,
				"adecl-"+adapter, adapter, adapter, at, at); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO provider_installations(
				provider_installation_id, schema_version, record_version, scope, project_id, adapter_id, adapter_declaration_id,
				provider_display_name, executable_name, executable_identity_json, canonical_path_redacted, discovery_source,
				discovery_order, platform, version_confidence, installation_state, usable_for_invocation, created_at,
				updated_at, captured_at, stale_after, freshness_state, confidence, side_effect_class, classification, payload_json)
				VALUES (?, 'loopcoder.provider_installation.v1', 1, 'project', 'proj-routing', ?, ?, ?, ?, '{}', ?, 'test',
				1, 'darwin/arm64', 'exact', 'active', 'yes', ?, ?, ?, ?, 'fresh', 'exact', 'local-read', 'local-diagnostic', '{}')
				ON CONFLICT(provider_installation_id) DO NOTHING`,
				"pinst-"+adapter, adapter, "adecl-"+adapter, adapter, adapter, adapter, at, at, at, fixture.now.Add(time.Hour).Format(time.RFC3339Nano)); err != nil {
				return err
			}
			accountID := "acct-a"
			modelID := "codex-good"
			if adapter == "claude" {
				accountID = "acct-c"
				modelID = "claude-good"
			}
			if _, err := tx.Exec(ctx, `INSERT INTO account_profiles(account_profile_id, adapter_id, provider_installation_id, payload_json)
				VALUES (?, ?, ?, '{}') ON CONFLICT(account_profile_id) DO NOTHING`, accountID, adapter, "pinst-"+adapter); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO model_catalog_snapshots(model_catalog_snapshot_id, adapter_id, provider_installation_id, payload_json)
				VALUES (?, ?, ?, '{}') ON CONFLICT(model_catalog_snapshot_id) DO NOTHING`, "mcats-"+adapter, adapter, "pinst-"+adapter); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO model_capabilities(model_capability_id, model_catalog_snapshot_id, adapter_id, payload_json)
				VALUES (?, ?, ?, '{}') ON CONFLICT(model_capability_id) DO NOTHING`, modelID, "mcats-"+adapter, adapter); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed provider inventory rows: %v", err)
	}
}

func seedHandoffAgentAuthority(t *testing.T, ctx context.Context, store storage.Store, fixture hardFixture, claim storage.ClaimResult) {
	t.Helper()
	sourceScope := budget.Scope{ScopeKind: budget.ScopeSubAgent, ProjectID: "proj-routing", DeliveryRunID: "drun-routing", TaskID: "task-a", SubAgentID: "agent-source", AdapterID: "codex", AccountProfileID: "acct-a", ModelCapabilityID: "codex-good"}
	sourcePolicy, err := budget.UpsertPolicy(ctx, store, budget.PolicyInput{
		Scope: sourceScope, QuantityKind: providerinventory.QuantityLocalPolicy, WindowKind: providerinventory.WindowUnbounded,
		PolicyMode: budget.PolicyHard, CeilingValue: 10, PolicyVersion: "handoff-source-test", Ordinal: "source",
		Actor: budget.Actor{ActorID: "test"}, Host: budget.Host{HostID: "test"},
	})
	if err != nil {
		t.Fatalf("upsert source policy: %v", err)
	}
	sourceReservation, err := budget.Reserve(ctx, store, budget.ReserveRequest{
		ScopeChain: []budget.Scope{sourceScope}, QuantityKind: providerinventory.QuantityLocalPolicy, WindowKind: providerinventory.WindowUnbounded,
		RequestedValue: 1, LeaseExpiresAt: fixture.now.Add(time.Hour), IdempotencyKey: "source-reservation", RequesterID: "agent-source",
		AuthorizationFingerprint: testFingerprint("auth"), RequirementConfidence: providerinventory.ConfidenceExact,
		Actor: budget.Actor{ActorID: "test"}, Host: budget.Host{HostID: "test"},
	})
	if err != nil {
		t.Fatalf("reserve source policy: %v", err)
	}
	at := delivery.CanonicalTimestamp(fixture.now)
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO agent_scope_grants(
			id, project_id, delivery_run_id, child_agent_id, schema_version, record_version, scope_json, permission,
			side_effect_class, policy_version, policy_fingerprint, plan_fingerprint, authorization_fingerprint,
			agent_federation_fingerprint, created_at, updated_at)
			VALUES ('scope-source', 'proj-routing', 'drun-routing', 'agent-source', 'loopcoder.agent_scope_grant.v1', 1, ?, ?, ?, 'test', ?, ?, ?, 'seed-fp', ?, ?)`,
			`{"paths":["README.md"],"repo":"proj-routing"}`,
			storage.PermissionReadOnly, string(taskrequirements.SideEffectLocalRead), testFingerprint("delivery-policy"), testFingerprint("plan-routing"), testFingerprint("auth"), at, at); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO agent_budget_bindings(
			id, project_id, delivery_run_id, child_agent_id, budget_policy_id, budget_reservation_id,
			reservation_scope, reserved_quantities_json, ancestor_budget_refs_json, reservation_state, created_at, updated_at)
			VALUES ('binding-source', 'proj-routing', 'drun-routing', 'agent-source', ?, ?, 'sub-agent', '{}', '[]', 'active', ?, ?)`,
			sourcePolicy.BudgetPolicyID, sourceReservation.Reservation.BudgetReservationID, at, at); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO agent_registrations(
			id, record_version, project_id, delivery_run_id, root_run_id, parent_run_id, child_run_id, task_id, attempt_id,
			plan_id, child_key, adapter_id, provider_installation_id, account_profile_id, model_capability_id,
			routing_decision_id, provider_session_ref, scope_grant_id, permission, side_effect_class, budget_binding_ids_json,
			ownership_lock_ids_json, claim_generation, executor_id, provider_idempotency_key, cancellation_channel,
			expected_outputs_json, registration_state, depth, policy_version, plan_fingerprint, policy_fingerprint,
			authorization_fingerprint, agent_federation_fingerprint, registration_payload_hash, created_at, updated_at)
			VALUES ('agent-source', 1, 'proj-routing', 'drun-routing', 'run-routing', 'run-routing', 'run-routing-child', 'task-a', 'attempt-source',
			'plan-child', 'worker', 'codex', 'pinst-codex', 'acct-a', 'codex-good', '', '', 'scope-source', ?, ?, '["binding-source"]',
			'[]', ?, ?, ?, 'cancel', '{}', 'registered', 1, 'test', ?, ?, ?, 'seed-fp', 'seed-payload', ?, ?)`,
			storage.PermissionReadOnly, string(taskrequirements.SideEffectLocalRead), claim.ClaimGeneration, claim.ExecutorID, claim.ProviderKey,
			testFingerprint("plan-routing"), testFingerprint("delivery-policy"), testFingerprint("auth"), at, at)
		return err
	}); err != nil {
		t.Fatalf("seed agent authority: %v", err)
	}
}

func countSuccessorAttempts(t *testing.T, ctx context.Context, store storage.Store, taskID string) int {
	t.Helper()
	var count int
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM delivery_attempts WHERE task_id = ? AND attempt_id <> 'attempt-source'`, taskID).Scan(&count)
	}); err != nil {
		t.Fatalf("count successor attempts: %v", err)
	}
	return count
}

func reservationState(t *testing.T, ctx context.Context, store storage.Store, reservationID string) string {
	t.Helper()
	var state string
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM budget_reservations WHERE budget_reservation_id = ?`, reservationID).Scan(&state)
	}); err != nil {
		t.Fatalf("reservation state: %v", err)
	}
	return state
}
