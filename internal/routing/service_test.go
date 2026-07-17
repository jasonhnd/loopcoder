package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/availability"
	"github.com/jasonhnd/loopcoder/internal/budget"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

func TestExplainStoredRouteIsReadOnlyAndRepeatable(t *testing.T) {
	ctx := context.Background()
	store, request := openStoredRouteServiceFixture(t, ctx)
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM routing_policy_profiles`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM role_definitions`)
		return err
	}); err != nil {
		t.Fatalf("delete routing profiles: %v", err)
	}

	before := routeServiceMutationCounts(t, ctx, store)
	first, err := ExplainStoredRoute(ctx, store, request)
	if err != nil {
		t.Fatalf("ExplainStoredRoute: %v", err)
	}
	second, err := ExplainStoredRoute(ctx, store, request)
	if err != nil {
		t.Fatalf("ExplainStoredRoute replay: %v", err)
	}
	if first.SchemaVersion != RouteOperationSchema || first.Operation != RouteOperationExplain || first.Outcome != RouteOutcomeSelected {
		t.Fatalf("explain result = %#v", first)
	}
	if first.Persisted || first.Replayed || first.ProviderCalls != 0 {
		t.Fatalf("explain side-effect metadata = %#v", first)
	}
	if first.Decision.RoutingDecisionID == "" || first.Decision.RoutingDecisionID != second.Decision.RoutingDecisionID || first.Decision.ChosenCandidateID != second.Decision.ChosenCandidateID {
		t.Fatalf("explain is not repeatable: first=%#v second=%#v", first.Decision, second.Decision)
	}
	if after := routeServiceMutationCounts(t, ctx, store); after != before {
		t.Fatalf("explain mutated durable routing state: before=%#v after=%#v", before, after)
	}
}

func TestDecideStoredRoutePersistsOneImmutableDecisionAndReplays(t *testing.T) {
	ctx := context.Background()
	store, request := openStoredRouteServiceFixture(t, ctx)

	first, err := DecideStoredRoute(ctx, store, request)
	if err != nil {
		t.Fatalf("DecideStoredRoute: %v", err)
	}
	if first.Outcome != RouteOutcomeSelected || !first.Persisted || first.Replayed || first.ProviderCalls != 0 {
		t.Fatalf("first decide result = %#v", first)
	}
	assertRoutingDecisionCount(t, ctx, store, request.ProjectID, request.DeliveryRunID, request.DecisionKey, 1)

	replayed, err := DecideStoredRoute(ctx, store, request)
	if err != nil {
		t.Fatalf("DecideStoredRoute replay: %v", err)
	}
	if !replayed.Replayed || replayed.Decision.RoutingDecisionID != first.Decision.RoutingDecisionID {
		t.Fatalf("replay result = %#v, first = %#v", replayed, first)
	}
	assertRoutingDecisionCount(t, ctx, store, request.ProjectID, request.DeliveryRunID, request.DecisionKey, 1)

	request.Pin = &CandidateConstraint{AdapterID: "claude"}
	request.PinReason = "attempt to replace immutable authority"
	_, err = DecideStoredRoute(ctx, store, request)
	if !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("changed replay pin error = %v, want ErrRoutingFingerprintMismatch", err)
	}
	assertRoutingDecisionCount(t, ctx, store, request.ProjectID, request.DeliveryRunID, request.DecisionKey, 1)
}

func TestDecideStoredRoutePersistsPinAndTypedNoRoute(t *testing.T) {
	ctx := context.Background()
	store, request := openStoredRouteServiceFixture(t, ctx)
	request.DecisionKey = "route-pinned-unavailable"
	request.Pin = &CandidateConstraint{AdapterID: "paseo"}
	request.PinReason = "operator requested a verifier-only fixture provider for worker routing"

	result, err := DecideStoredRoute(ctx, store, request)
	assertRouteServiceErrorCode(t, err, taskrequirements.ErrPinnedCandidateIneligibleCode)
	if result.Outcome != RouteOutcomeNoRoute || !result.Persisted || result.Replayed || result.ProviderCalls != 0 {
		t.Fatalf("no-route result = %#v", result)
	}
	if result.Decision.DecisionStatus != DecisionStatusNoEligible || result.Decision.TerminalErrorCode != taskrequirements.ErrPinnedCandidateIneligibleCode {
		t.Fatalf("no-route decision = %#v", result.Decision)
	}
	if got := routeServiceTableCount(t, ctx, store, "routing_policy_inputs"); got != 1 {
		t.Fatalf("routing policy input count = %d, want 1", got)
	}
	assertRoutingDecisionCount(t, ctx, store, request.ProjectID, request.DeliveryRunID, request.DecisionKey, 1)

	replayed, replayErr := DecideStoredRoute(ctx, store, request)
	assertRouteServiceErrorCode(t, replayErr, taskrequirements.ErrPinnedCandidateIneligibleCode)
	if !replayed.Replayed || replayed.Decision.RoutingDecisionID != result.Decision.RoutingDecisionID {
		t.Fatalf("no-route replay = %#v, first = %#v", replayed, result)
	}

	fixture := newFixture(t)
	otherRequirement := decisionRequirement(t, fixture, workerRequirement("task-b"), "treq-route-service-b", testFingerprint("plan-routing"))
	otherRequirement = persistFallbackTaskRequirement(t, ctx, store, otherRequirement, fixture.now)
	otherRequest := request
	otherRequest.TaskRequirementID = otherRequirement.TaskRequirementID
	otherRequest.DecisionKey = "route-other-task"
	otherRequest.Pin = nil
	otherRequest.PinReason = ""
	other, otherErr := ExplainStoredRoute(ctx, store, otherRequest)
	if otherErr != nil || other.Outcome != RouteOutcomeSelected {
		t.Fatalf("task-scoped pin affected another task: result=%#v err=%v", other, otherErr)
	}
	if len(other.Decision.PolicyInputRecords) != 0 || len(other.Decision.UserPinRefs) != 0 {
		t.Fatalf("other task inherited task-scoped pin: %#v", other.Decision.PolicyInputRecords)
	}
}

func TestStoredRouteExplicitPinOverridesLegacyTaskPinWithoutLeakingAcrossDecisionKeys(t *testing.T) {
	ctx := context.Background()
	store, request := openStoredRouteServiceFixture(t, ctx)
	profile, ok := BuiltInRoutingPolicyProfile(ProfileKeyBalanced, store.Now())
	if !ok {
		t.Fatal("balanced routing profile is unavailable")
	}
	legacy, err := PersistPolicyInput(ctx, store, PolicyInputRecord{
		InputKind:              PolicyInputKindPin,
		ProjectID:              request.ProjectID,
		DeliveryRunID:          request.DeliveryRunID,
		RoutingPolicyProfileID: profile.RoutingPolicyProfileID,
		PolicyFingerprint:      profile.PolicyFingerprint,
		Scope:                  "task:task-a",
		Reason:                 "legacy task-wide route",
		Constraint:             CandidateConstraint{AdapterID: "claude"},
		Actor:                  userActor(),
		Host:                   routingHost(),
	})
	if err != nil {
		t.Fatalf("PersistPolicyInput legacy pin: %v", err)
	}

	explicit := request
	explicit.DecisionKey = "route-explicit-codex"
	explicit.Pin = &CandidateConstraint{AdapterID: "codex", ModelCapabilityID: "codex-good"}
	explicit.PinReason = "exact provider requested for this execution attempt"
	result, err := DecideStoredRoute(ctx, store, explicit)
	if err != nil {
		t.Fatalf("DecideStoredRoute explicit pin: %v decision=%#v", err, result.Decision)
	}
	if got := chosenAdapterID(result.Decision); got != "codex" {
		t.Fatalf("explicit route adapter = %q, want codex", got)
	}
	if len(result.Decision.PolicyInputRecords) != 1 || result.Decision.PolicyInputRecords[0].DecisionKey != explicit.DecisionKey {
		t.Fatalf("explicit decision policy inputs = %#v, want only decision-scoped pin", result.Decision.PolicyInputRecords)
	}
	if result.Decision.PolicyInputRecords[0].RoutingPolicyInputID == legacy.RoutingPolicyInputID {
		t.Fatalf("explicit decision retained legacy task pin: %#v", result.Decision.PolicyInputRecords)
	}

	legacyRequest := request
	legacyRequest.DecisionKey = "route-legacy-claude"
	legacyResult, err := ExplainStoredRoute(ctx, store, legacyRequest)
	if err != nil {
		t.Fatalf("ExplainStoredRoute legacy pin: %v", err)
	}
	if legacyResult.Outcome != RouteOutcomeNoRoute {
		t.Fatalf("legacy claude pin outcome = %q, want no_route", legacyResult.Outcome)
	}
	if len(legacyResult.Decision.PolicyInputRecords) != 1 || legacyResult.Decision.PolicyInputRecords[0].RoutingPolicyInputID != legacy.RoutingPolicyInputID {
		t.Fatalf("legacy decision policy inputs = %#v, want only legacy task pin", legacyResult.Decision.PolicyInputRecords)
	}
}

func TestStoredRouteRejectsUnknownPinBeforePersistingIt(t *testing.T) {
	ctx := context.Background()
	store, request := openStoredRouteServiceFixture(t, ctx)
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM routing_policy_profiles`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM role_definitions`)
		return err
	}); err != nil {
		t.Fatalf("delete built-in route metadata: %v", err)
	}
	request.DecisionKey = "route-unknown-pin"
	request.Pin = &CandidateConstraint{AdapterID: "not-a-provider"}
	request.PinReason = "invalid fixture pin"
	before := routeServiceTableCount(t, ctx, store, "routing_policy_inputs")

	_, err := DecideStoredRoute(ctx, store, request)
	assertRouteServiceErrorCode(t, err, taskrequirements.ErrMissingReferenceCode)
	if after := routeServiceTableCount(t, ctx, store, "routing_policy_inputs"); after != before {
		t.Fatalf("unknown pin was persisted: before=%d after=%d", before, after)
	}
	if counts := routeServiceMutationCounts(t, ctx, store); counts.Roles != 0 || counts.Profiles != 0 {
		t.Fatalf("failed decide did not roll back built-in metadata: %#v", counts)
	}
	assertRoutingDecisionCount(t, ctx, store, request.ProjectID, request.DeliveryRunID, request.DecisionKey, 0)

	request.DecisionKey = "route-unknown-model-pin"
	request.Pin = &CandidateConstraint{ModelCapabilityID: "not-a-model"}
	_, err = DecideStoredRoute(ctx, store, request)
	assertRouteServiceErrorCode(t, err, taskrequirements.ErrMissingReferenceCode)
	if after := routeServiceTableCount(t, ctx, store, "routing_policy_inputs"); after != before {
		t.Fatalf("unknown model pin was persisted: before=%d after=%d", before, after)
	}
	assertRoutingDecisionCount(t, ctx, store, request.ProjectID, request.DeliveryRunID, request.DecisionKey, 0)
}

func TestConcurrentDecideStoredRouteCommitsOnePinAndFirstDecision(t *testing.T) {
	ctx := context.Background()
	store, request := openStoredRouteServiceFixture(t, ctx)
	request.DecisionKey = "route-concurrent-first"
	request.PinReason = "concurrent explicit route"
	requests := []StoredRouteRequest{request, request}
	requests[0].Pin = &CandidateConstraint{AdapterID: "codex", ModelCapabilityID: "codex-good"}
	requests[1].Pin = &CandidateConstraint{AdapterID: "claude", ModelCapabilityID: "claude-good"}

	type outcome struct {
		result RouteOperationResult
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, len(requests))
	for _, current := range requests {
		current := current
		go func() {
			<-start
			result, err := DecideStoredRoute(ctx, store, current)
			outcomes <- outcome{result: result, err: err}
		}()
	}
	close(start)
	var committed, mismatched int
	for range requests {
		got := <-outcomes
		switch {
		case got.result.Persisted && !got.result.Replayed && got.result.Decision.RoutingDecisionID != "":
			if got.err != nil {
				var typed *taskrequirements.TypedError
				if !errors.As(got.err, &typed) || typed.Code != taskrequirements.ErrPinnedCandidateIneligibleCode {
					t.Fatalf("committed concurrent decision returned unexpected error: %v", got.err)
				}
			}
			committed++
		case errors.Is(got.err, taskrequirements.ErrRoutingFingerprintMismatch):
			mismatched++
		default:
			t.Fatalf("concurrent decide outcome = %#v err=%v", got.result, got.err)
		}
	}
	if committed != 1 || mismatched != 1 {
		t.Fatalf("concurrent outcomes: committed=%d mismatched=%d", committed, mismatched)
	}
	if got := routeServiceTableCount(t, ctx, store, "routing_policy_inputs"); got != 1 {
		t.Fatalf("concurrent pin count = %d, want 1", got)
	}
	assertRoutingDecisionCount(t, ctx, store, request.ProjectID, request.DeliveryRunID, request.DecisionKey, 1)
}

func TestExplainStoredRouteSurfacesStaleUnknownAndDisabledEvidence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(*testing.T, context.Context, storage.Store)
		wantReason RejectionCode
	}{
		{
			name: "stale telemetry",
			mutate: func(t *testing.T, ctx context.Context, store storage.Store) {
				mutateRouteQuotaEvidence(t, ctx, store, providerinventory.ConfidenceStale, providerinventory.FreshnessStale)
			},
			wantReason: RejectEvidenceStale,
		},
		{
			name: "unknown telemetry",
			mutate: func(t *testing.T, ctx context.Context, store storage.Store) {
				mutateRouteQuotaEvidence(t, ctx, store, providerinventory.ConfidenceUnknown, providerinventory.FreshnessFresh)
			},
			wantReason: RejectUnknownTelemetry,
		},
		{
			name:       "provider disabled",
			mutate:     mutateRouteInstallationsUnavailable,
			wantReason: RejectAvailabilityHardIneligible,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, request := openStoredRouteServiceFixture(t, ctx)
			request.DecisionKey = "route-evidence-" + strings.ReplaceAll(tc.name, " ", "-")
			tc.mutate(t, ctx, store)
			result, err := ExplainStoredRoute(ctx, store, request)
			if err != nil {
				t.Fatalf("ExplainStoredRoute: %v", err)
			}
			if result.Outcome != RouteOutcomeNoRoute || result.Decision.TerminalErrorCode != taskrequirements.ErrNoEligibleCandidateCode {
				t.Fatalf("evidence result = %#v", result)
			}
			if !routeDecisionHasRejection(result.Decision, tc.wantReason) {
				t.Fatalf("rejections = %#v, want %s", result.Decision.RejectedCandidates, tc.wantReason)
			}
		})
	}
}

func TestExplainStoredRouteSurfacesUnsupportedPermission(t *testing.T) {
	ctx := context.Background()
	store, request := openStoredRouteServiceFixture(t, ctx)
	fixture := newFixture(t)
	requirement := workerRequirement("task-orchestrate")
	requirement.PermissionRequired = taskrequirements.PermissionOrchestrate
	requirement.NestedAllowed = true
	requirement = decisionRequirement(t, fixture, requirement, "treq-route-orchestrate", testFingerprint("plan-routing"))
	requirement = persistFallbackTaskRequirement(t, ctx, store, requirement, fixture.now)
	request.TaskRequirementID = requirement.TaskRequirementID
	request.DecisionKey = "route-unsupported-permission"

	result, err := ExplainStoredRoute(ctx, store, request)
	if err != nil {
		t.Fatalf("ExplainStoredRoute: %v", err)
	}
	if result.Outcome != RouteOutcomeNoRoute || !routeDecisionHasRejection(result.Decision, RejectPermissionUnsupported) {
		t.Fatalf("unsupported permission result = %#v", result)
	}
}

func TestStoredRouteValidatesAndBindsBudgetAndDeadlineClassesOnReplay(t *testing.T) {
	ctx := context.Background()
	store, request := openStoredRouteServiceFixture(t, ctx)
	request.DecisionKey = "route-explicit-task-fit-classes"
	request.BudgetClass = BudgetClassMedium
	request.DeadlineClass = DeadlineClassShort

	first, err := DecideStoredRoute(ctx, store, request)
	if err != nil {
		t.Fatalf("DecideStoredRoute explicit classes: %v", err)
	}
	if first.Decision.BudgetClass != BudgetClassMedium || first.Decision.DeadlineClass != DeadlineClassShort {
		t.Fatalf("persisted classes = %q/%q", first.Decision.BudgetClass, first.Decision.DeadlineClass)
	}
	replay, err := DecideStoredRoute(ctx, store, request)
	if err != nil || !replay.Replayed || replay.Decision.RoutingDecisionID != first.Decision.RoutingDecisionID {
		t.Fatalf("same-class replay = %#v err=%v", replay, err)
	}

	changed := request
	changed.DeadlineClass = DeadlineClassMedium
	if _, err := DecideStoredRoute(ctx, store, changed); !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("changed deadline replay error = %v, want ErrRoutingFingerprintMismatch", err)
	}
	changed = request
	changed.BudgetClass = BudgetClassShort
	if _, err := DecideStoredRoute(ctx, store, changed); !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("changed budget replay error = %v, want ErrRoutingFingerprintMismatch", err)
	}

	invalid := request
	invalid.DecisionKey = "route-invalid-task-fit-class"
	invalid.BudgetClass = BudgetClass("unbounded")
	if _, err := ExplainStoredRoute(ctx, store, invalid); !errors.Is(err, taskrequirements.ErrInvalidRecord) {
		t.Fatalf("invalid budget class error = %v, want ErrInvalidRecord", err)
	}
	invalid.BudgetClass = ""
	invalid.DeadlineClass = DeadlineClass("instant")
	if _, err := ExplainStoredRoute(ctx, store, invalid); !errors.Is(err, taskrequirements.ErrInvalidRecord) {
		t.Fatalf("invalid deadline class error = %v, want ErrInvalidRecord", err)
	}
}

type routeServiceCounts struct {
	Roles     int
	Profiles  int
	Inputs    int
	Decisions int
}

func openStoredRouteServiceFixture(t *testing.T, ctx context.Context) (storage.Store, StoredRouteRequest) {
	t.Helper()
	fixture := newFixture(t)
	store, err := storage.Open(ctx, storage.Options{Path: tempDB(t), Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("open route service store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedRoutingDecisionStore(t, ctx, store, fixture.now)

	requirement := decisionRequirement(t, fixture, workerRequirement("task-a"), "treq-route-service", testFingerprint("plan-routing"))
	requirement = persistFallbackTaskRequirement(t, ctx, store, requirement, fixture.now)

	scores := fixture.availabilityScores()
	for i := range scores {
		scores[i].AvailabilityScoreID = ""
		scores[i].Score = 90
		scores[i].ScoreConfidence = providerinventory.ConfidenceExact
		scores[i].Components = []availability.Component{{
			Name: "fixture-health", Score: 90, MaxScore: 100, Confidence: providerinventory.ConfidenceExact,
			FreshnessState: providerinventory.FreshnessFresh, Explanation: "deterministic route service fixture",
		}}
		scores[i].CapturedAt = fixture.now.Format(time.RFC3339Nano)
	}
	if err := availability.Persist(ctx, store, availability.Result{Scores: scores}); err != nil {
		t.Fatalf("persist availability fixture: %v", err)
	}
	for i, summary := range fixture.budgets {
		_, err := budget.UpsertPolicy(ctx, store, budget.PolicyInput{
			Scope: budget.Scope{
				ScopeKind: budget.ScopeProvider, ProjectID: "proj-routing", AdapterID: summary.Scope.AdapterID,
				AccountProfileID: summary.Scope.AccountProfileID, ModelCapabilityID: summary.Scope.ModelCapabilityID,
			},
			QuantityKind: providerinventory.QuantityLocalPolicy, WindowKind: providerinventory.WindowUnbounded,
			PolicyMode: budget.PolicyHard, CeilingValue: summary.AvailableValue, PolicyVersion: "route-service-test-v1",
			Ordinal: fmt.Sprintf("%d", i), Actor: budget.Actor{ActorID: "route-test", Role: "test"},
			Host: budget.Host{HostID: "routing-test"}, Source: "fixture", Evidence: "deterministic fixture",
		})
		if err != nil {
			t.Fatalf("persist budget fixture: %v", err)
		}
	}

	return store, StoredRouteRequest{
		ProjectID: "proj-routing", DeliveryRunID: "drun-routing", TaskRequirementID: requirement.TaskRequirementID,
		DecisionKey: "route-service-worker", RoutingPolicyProfileKey: ProfileKeyBalanced, HostName: "codex-cli",
		PinActor: userActor(), DecidedBy: routerActor(), Host: routingHost(),
	}
}

func routeServiceMutationCounts(t *testing.T, ctx context.Context, store storage.Store) routeServiceCounts {
	t.Helper()
	return routeServiceCounts{
		Roles:     routeServiceTableCount(t, ctx, store, "role_definitions"),
		Profiles:  routeServiceTableCount(t, ctx, store, "routing_policy_profiles"),
		Inputs:    routeServiceTableCount(t, ctx, store, "routing_policy_inputs"),
		Decisions: routeServiceTableCount(t, ctx, store, "routing_decisions"),
	}
}

func routeServiceTableCount(t *testing.T, ctx context.Context, store storage.Store, table string) int {
	t.Helper()
	query := ""
	switch table {
	case "role_definitions":
		query = `SELECT COUNT(*) FROM role_definitions`
	case "routing_policy_profiles":
		query = `SELECT COUNT(*) FROM routing_policy_profiles`
	case "routing_policy_inputs":
		query = `SELECT COUNT(*) FROM routing_policy_inputs`
	case "routing_decisions":
		query = `SELECT COUNT(*) FROM routing_decisions`
	default:
		t.Fatalf("unsupported route service table %q", table)
	}
	var count int
	if err := store.WithTx(ctx, func(tx storage.Tx) error { return tx.QueryRow(ctx, query).Scan(&count) }); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

func assertRouteServiceErrorCode(t *testing.T, err error, want taskrequirements.ErrorCode) {
	t.Helper()
	var typed *taskrequirements.TypedError
	if !errors.As(err, &typed) || typed.Code != want {
		t.Fatalf("route error = %T %v, want code %s", err, err, want)
	}
}

func routeDecisionHasRejection(decision RoutingDecision, want RejectionCode) bool {
	for _, rejected := range decision.RejectedCandidates {
		for _, reason := range rejected.Reasons {
			if reason.Code == want {
				return true
			}
		}
	}
	return false
}

func chosenAdapterID(decision RoutingDecision) string {
	for _, candidate := range decision.EligibleCandidates {
		if candidate.RoutingCandidateID == decision.ChosenCandidateID {
			return candidate.AdapterID
		}
	}
	return ""
}

func mutateRouteQuotaEvidence(t *testing.T, ctx context.Context, store storage.Store, confidence providerinventory.Confidence, freshness providerinventory.FreshnessState) {
	t.Helper()
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, `SELECT quota_snapshot_id, payload_json FROM quota_snapshots ORDER BY quota_snapshot_id`)
		if err != nil {
			return err
		}
		type row struct{ id, payload string }
		var values []row
		for rows.Next() {
			var value row
			if err := rows.Scan(&value.id, &value.payload); err != nil {
				rows.Close()
				return err
			}
			values = append(values, value)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, value := range values {
			var snapshot providerinventory.QuotaSnapshot
			if err := json.Unmarshal([]byte(value.payload), &snapshot); err != nil {
				return err
			}
			snapshot.Confidence = confidence
			snapshot.FreshnessState = freshness
			payload, err := json.Marshal(snapshot)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE quota_snapshots SET confidence = ?, freshness_state = ?, payload_json = ? WHERE quota_snapshot_id = ?`, confidence, freshness, string(payload), value.id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("mutate route quota evidence: %v", err)
	}
}

func mutateRouteInstallationsUnavailable(t *testing.T, ctx context.Context, store storage.Store) {
	t.Helper()
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, `SELECT provider_installation_id, payload_json FROM provider_installations ORDER BY provider_installation_id`)
		if err != nil {
			return err
		}
		type row struct{ id, payload string }
		var values []row
		for rows.Next() {
			var value row
			if err := rows.Scan(&value.id, &value.payload); err != nil {
				rows.Close()
				return err
			}
			values = append(values, value)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, value := range values {
			var installation providerinventory.ProviderInstallation
			if err := json.Unmarshal([]byte(value.payload), &installation); err != nil {
				return err
			}
			installation.InstallationState = providerinventory.InstallationNotInstalled
			installation.UsableForInvocation = "no"
			payload, err := json.Marshal(installation)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE provider_installations SET installation_state = ?, usable_for_invocation = ?, payload_json = ? WHERE provider_installation_id = ?`, installation.InstallationState, installation.UsableForInvocation, string(payload), value.id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("mutate route installations: %v", err)
	}
}
