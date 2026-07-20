package routing

import (
	"context"
	"errors"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

func TestStoredRouteAuthorityClaimAndReplay(t *testing.T) {
	ctx := context.Background()
	store, request := openStoredRouteServiceFixture(t, ctx)

	first, err := DecideStoredRoute(ctx, store, request)
	if err != nil {
		t.Fatalf("DecideStoredRoute first: %v", err)
	}
	assertRouteAttemptAuthorityCount(t, ctx, store, request, 1)
	authority, found, err := loadRouteAttemptAuthority(ctx, store, request.ProjectID, request.DeliveryRunID, request.DecisionKey, request.HostName)
	if err != nil || !found {
		t.Fatalf("loadRouteAttemptAuthority = found %t err %v", found, err)
	}
	if authority.RoutingDecisionID != first.Decision.RoutingDecisionID {
		t.Fatalf("authority decision = %s, want %s", authority.RoutingDecisionID, first.Decision.RoutingDecisionID)
	}

	replayed, err := DecideStoredRoute(ctx, store, request)
	if err != nil {
		t.Fatalf("DecideStoredRoute replay: %v", err)
	}
	if !replayed.Replayed || replayed.Decision.RoutingDecisionID != first.Decision.RoutingDecisionID {
		t.Fatalf("replayed authority = %#v, first = %#v", replayed, first)
	}
	assertRouteAttemptAuthorityCount(t, ctx, store, request, 1)
}

func TestExplainStoredRouteBindsFirstAuthorityWhileExplainingCurrentState(t *testing.T) {
	ctx := context.Background()
	store, request := openStoredRouteServiceFixture(t, ctx)
	profile, ok := BuiltInRoutingPolicyProfile(request.RoutingPolicyProfileKey, store.Now())
	if !ok {
		t.Fatal("built-in route profile is unavailable")
	}
	input, err := assembleStoredRouteInput(ctx, store, request, profile)
	if err != nil {
		t.Fatalf("assembleStoredRouteInput: %v", err)
	}
	first, err := DecideStoredRoute(ctx, store, request)
	if err != nil {
		t.Fatalf("DecideStoredRoute first: %v", err)
	}

	mutateRouteInstallationsUnavailable(t, ctx, store)
	reexamined, reexamineErr := ReevaluateRoute(ctx, store, ReevaluateRouteInput{
		DecisionInput: input,
		Trigger:       ReevaluateAtFreshCapacityEvent,
	})
	if reexamineErr != nil && !errors.Is(reexamineErr, taskrequirements.ErrNoEligibleCandidate) {
		t.Fatalf("ReevaluateRoute history: %v", reexamineErr)
	}
	if reexamined.Decision.RoutingDecisionID == "" || reexamined.Decision.RoutingDecisionID == first.Decision.RoutingDecisionID {
		t.Fatalf("re-evaluation did not append distinct history: first=%s next=%s", first.Decision.RoutingDecisionID, reexamined.Decision.RoutingDecisionID)
	}
	assertRoutingDecisionCount(t, ctx, store, request.ProjectID, request.DeliveryRunID, request.DecisionKey, 2)

	explained, err := ExplainStoredRoute(ctx, store, request)
	if err != nil {
		t.Fatalf("ExplainStoredRoute authority: %v", err)
	}
	if explained.Persisted || explained.Replayed || explained.PriorRoutingDecisionID != first.Decision.RoutingDecisionID ||
		explained.Decision.RoutingDecisionID == first.Decision.RoutingDecisionID || explained.Outcome != RouteOutcomeNoRoute ||
		!hasInputRecordRef(explained.Decision.InputRecordRefs, "prior_routing_decision", first.Decision.RoutingDecisionID) {
		t.Fatalf("explain did not bind first authority while calculating current state: explained=%#v first=%#v", explained, first)
	}
	replayed, err := DecideStoredRoute(ctx, store, request)
	if err != nil {
		t.Fatalf("DecideStoredRoute after history: %v", err)
	}
	if !replayed.Replayed || replayed.Decision.RoutingDecisionID != first.Decision.RoutingDecisionID {
		t.Fatalf("decide followed re-evaluation history instead of authority: %#v", replayed)
	}
	assertRouteAttemptAuthorityCount(t, ctx, store, request, 1)
}

func TestStoredRouteAuthorityAdoptsSingleLegacyDecision(t *testing.T) {
	ctx := context.Background()
	store, request := openStoredRouteServiceFixture(t, ctx)
	legacy, _ := persistLegacyStoredRouteDecision(t, ctx, store, request)
	assertRouteAttemptAuthorityCount(t, ctx, store, request, 0)

	result, err := DecideStoredRoute(ctx, store, request)
	if err != nil {
		t.Fatalf("DecideStoredRoute legacy adoption: %v", err)
	}
	if !result.Replayed || result.Decision.RoutingDecisionID != legacy.RoutingDecisionID {
		t.Fatalf("legacy adoption result = %#v, legacy = %#v", result, legacy)
	}
	assertRouteAttemptAuthorityCount(t, ctx, store, request, 1)
}

func TestStoredRouteReplayRejectsSameProfileIDWithChangedPolicyFingerprint(t *testing.T) {
	ctx := context.Background()
	store, request := openStoredRouteServiceFixture(t, ctx)
	first, err := DecideStoredRoute(ctx, store, request)
	if err != nil {
		t.Fatalf("DecideStoredRoute first: %v", err)
	}
	profile, ok := BuiltInRoutingPolicyProfile(request.RoutingPolicyProfileKey, store.Now())
	if !ok {
		t.Fatal("built-in route profile is unavailable")
	}
	profile.PolicyFingerprint = testFingerprint("changed-policy-same-profile-id")
	if err := validateStoredRouteReplay(ctx, store, request, profile, first.Decision); !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("changed policy fingerprint replay error = %v, want ErrRoutingFingerprintMismatch", err)
	}
}

func TestStoredRouteReplayRejectsChangedRuntimeHost(t *testing.T) {
	ctx := context.Background()
	store, request := openStoredRouteServiceFixture(t, ctx)
	first, err := DecideStoredRoute(ctx, store, request)
	if err != nil {
		t.Fatalf("DecideStoredRoute first: %v", err)
	}
	if first.Decision.RuntimeHostName != request.HostName {
		t.Fatalf("persisted runtime host = %q, want %q", first.Decision.RuntimeHostName, request.HostName)
	}

	changed := request
	changed.HostName = "generic-local"
	if changed.HostName == request.HostName {
		t.Fatal("changed runtime host fixture did not change")
	}
	if _, err := DecideStoredRoute(ctx, store, changed); !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("changed runtime host replay error = %v, want ErrRoutingFingerprintMismatch", err)
	}
	assertRouteAttemptAuthorityCount(t, ctx, store, request, 1)
	assertRoutingDecisionCount(t, ctx, store, request.ProjectID, request.DeliveryRunID, request.DecisionKey, 1)
}

func TestStoredRouteAuthorityRejectsTamperedRuntimeHostPayload(t *testing.T) {
	ctx := context.Background()
	store, request := openStoredRouteServiceFixture(t, ctx)
	first, err := DecideStoredRoute(ctx, store, request)
	if err != nil {
		t.Fatalf("DecideStoredRoute first: %v", err)
	}
	tampered := first.Decision
	tampered.RuntimeHostName = "generic-local"
	if tampered.RuntimeHostName == request.HostName {
		t.Fatal("tampered runtime host fixture did not change")
	}
	payload, err := delivery.CanonicalJSON(tampered)
	if err != nil {
		t.Fatalf("canonical tampered routing decision: %v", err)
	}
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE routing_decisions SET payload_json = ? WHERE routing_decision_id = ?`,
			string(payload), first.Decision.RoutingDecisionID)
		return err
	}); err != nil {
		t.Fatalf("tamper routing decision runtime host: %v", err)
	}

	if _, err := DecideStoredRoute(ctx, store, request); !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("tampered runtime host replay error = %v, want ErrRoutingFingerprintMismatch", err)
	}
	assertRouteAttemptAuthorityCount(t, ctx, store, request, 1)
	assertRoutingDecisionCount(t, ctx, store, request.ProjectID, request.DeliveryRunID, request.DecisionKey, 1)
}

func TestStoredRouteAuthorityRejectsLegacyRowAndPayloadIdentityMismatch(t *testing.T) {
	ctx := context.Background()
	store, request := openStoredRouteServiceFixture(t, ctx)
	legacy, _ := persistLegacyStoredRouteDecision(t, ctx, store, request)
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE routing_decisions SET routing_decision_id = ? WHERE routing_decision_id = ?`,
			"rdec_corrupt_row_identity", legacy.RoutingDecisionID)
		return err
	}); err != nil {
		t.Fatalf("corrupt legacy row identity: %v", err)
	}

	if _, err := DecideStoredRoute(ctx, store, request); !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("legacy row/payload mismatch error = %v, want ErrRoutingFingerprintMismatch", err)
	}
	assertRouteAttemptAuthorityCount(t, ctx, store, request, 0)
}

func TestStoredRouteAuthorityFailsClosedForAmbiguousLegacyHistory(t *testing.T) {
	ctx := context.Background()
	store, request := openStoredRouteServiceFixture(t, ctx)
	first, input := persistLegacyStoredRouteDecision(t, ctx, store, request)
	input.Inputs.HostName = "generic-local"
	second, err := BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision second legacy record: %v", err)
	}
	if second.RoutingDecisionID == first.RoutingDecisionID {
		t.Fatal("ambiguous legacy fixture did not create a distinct routing decision")
	}
	if err := PersistRoutingDecision(ctx, store, second); err != nil {
		t.Fatalf("PersistRoutingDecision second legacy record: %v", err)
	}
	assertRoutingDecisionCount(t, ctx, store, request.ProjectID, request.DeliveryRunID, request.DecisionKey, 2)

	if _, err := ExplainStoredRoute(ctx, store, request); !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("ExplainStoredRoute ambiguous legacy error = %v, want ErrRoutingFingerprintMismatch", err)
	}
	if _, err := DecideStoredRoute(ctx, store, request); !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("DecideStoredRoute ambiguous legacy error = %v, want ErrRoutingFingerprintMismatch", err)
	}
	assertRouteAttemptAuthorityCount(t, ctx, store, request, 0)
}

func TestStoredRouteAuthorityFailsClosedForMissingDecisionReference(t *testing.T) {
	ctx := context.Background()
	store, request := openStoredRouteServiceFixture(t, ctx)
	if err := claimRouteAttemptAuthority(ctx, store, RoutingDecision{
		ProjectID:         request.ProjectID,
		DeliveryRunID:     request.DeliveryRunID,
		DecisionKey:       request.DecisionKey,
		RuntimeHostName:   request.HostName,
		RoutingDecisionID: "rdec_missing_authority_target",
	}); err != nil {
		t.Fatalf("seed corrupt route authority: %v", err)
	}

	if _, err := ExplainStoredRoute(ctx, store, request); !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("ExplainStoredRoute corrupt authority error = %v, want ErrRoutingFingerprintMismatch", err)
	}
	if _, err := DecideStoredRoute(ctx, store, request); !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("DecideStoredRoute corrupt authority error = %v, want ErrRoutingFingerprintMismatch", err)
	}
	assertRoutingDecisionCount(t, ctx, store, request.ProjectID, request.DeliveryRunID, request.DecisionKey, 0)
	assertRouteAttemptAuthorityCount(t, ctx, store, request, 1)
}

func TestStoredRouteAuthorityFailsClosedForCorruptAuthorityPayload(t *testing.T) {
	ctx := context.Background()
	store, request := openStoredRouteServiceFixture(t, ctx)
	if _, err := DecideStoredRoute(ctx, store, request); err != nil {
		t.Fatalf("DecideStoredRoute seed authority: %v", err)
	}
	identity := routeAttemptIdentityFor(request.ProjectID, request.DeliveryRunID, request.DecisionKey, request.HostName)
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE delivery_idempotency SET result_json = ? WHERE idempotency_key = ?`,
			`{}`, routeAttemptAuthorityKey(identity))
		return err
	}); err != nil {
		t.Fatalf("corrupt route authority payload: %v", err)
	}

	if _, err := ExplainStoredRoute(ctx, store, request); !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("ExplainStoredRoute corrupt payload error = %v, want ErrRoutingFingerprintMismatch", err)
	}
	if _, err := DecideStoredRoute(ctx, store, request); !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("DecideStoredRoute corrupt payload error = %v, want ErrRoutingFingerprintMismatch", err)
	}
	assertRoutingDecisionCount(t, ctx, store, request.ProjectID, request.DeliveryRunID, request.DecisionKey, 1)
	assertRouteAttemptAuthorityCount(t, ctx, store, request, 1)
}

func persistLegacyStoredRouteDecision(t *testing.T, ctx context.Context, store storage.Store, request StoredRouteRequest) (RoutingDecision, DecisionInput) {
	t.Helper()
	profile, ok := BuiltInRoutingPolicyProfile(request.RoutingPolicyProfileKey, store.Now())
	if !ok {
		t.Fatal("built-in route profile is unavailable")
	}
	input, err := assembleStoredRouteInput(ctx, store, request, profile)
	if err != nil {
		t.Fatalf("assembleStoredRouteInput legacy: %v", err)
	}
	decision, err := BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision legacy: %v", err)
	}
	if err := PersistRoutingDecision(ctx, store, decision); err != nil {
		t.Fatalf("PersistRoutingDecision legacy: %v", err)
	}
	return decision, input
}

func assertRouteAttemptAuthorityCount(t *testing.T, ctx context.Context, store storage.Store, request StoredRouteRequest, want int) {
	t.Helper()
	identity := routeAttemptIdentityFor(request.ProjectID, request.DeliveryRunID, request.DecisionKey, request.HostName)
	var count int
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM delivery_idempotency WHERE idempotency_key = ? AND operation = ?`,
			routeAttemptAuthorityKey(identity), routeAttemptAuthorityOperation).Scan(&count)
	}); err != nil {
		t.Fatalf("count route attempt authority: %v", err)
	}
	if count != want {
		t.Fatalf("route attempt authority count = %d, want %d", count, want)
	}
}
