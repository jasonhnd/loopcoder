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
	releaseRegressionFixtureSchemaV1 = "loopcoder.evaluation.release_regression_fixture.v3"
	releaseRegressionPolicyVersion   = "simulation-release-policy-v3"
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
	invariantToScenarios := map[string][]string{}
	for _, invariant := range matrix.Invariants {
		invariantToScenarios[invariant.InvariantID] = append([]string(nil), invariant.Scenarios...)
		sort.Strings(invariantToScenarios[invariant.InvariantID])
	}
	scenarioToInvariants := map[string][]string{}
	for _, fixture := range matrix.Scenarios {
		scenarioToInvariants[fixture.ScenarioID] = append([]string(nil), fixture.Invariants...)
		sort.Strings(scenarioToInvariants[fixture.ScenarioID])
	}
	for scenarioID, invariantIDs := range scenarioToInvariants {
		for _, invariantID := range invariantIDs {
			if !containsString(invariantToScenarios[invariantID], scenarioID) {
				t.Fatalf("scenario %s claims invariant %s, but invariant does not list scenario", scenarioID, invariantID)
			}
		}
	}
	for invariantID, scenarioIDs := range invariantToScenarios {
		for _, scenarioID := range scenarioIDs {
			if !containsString(scenarioToInvariants[scenarioID], invariantID) {
				t.Fatalf("invariant %s claims scenario %s, but scenario does not list invariant", invariantID, scenarioID)
			}
		}
	}
	if !sameStringSlice(keys(knownScenarioIDs), keysFuncMap(scenarios)) {
		t.Fatalf("matrix scenario set mismatch\ngot:  %v\nwant: %v", keys(knownScenarioIDs), keysFuncMap(scenarios))
	}
}

func TestReleaseRegressionSnapshotsStable(t *testing.T) {
	for _, scenarioID := range keysFuncMap(releaseRegressionScenarios()) {
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

func TestReleaseRegressionSnapshotFilesMatchScenarioSet(t *testing.T) {
	want := map[string]bool{}
	for scenarioID := range releaseRegressionScenarios() {
		want[scenarioID+".canonical.json"] = true
		want[scenarioID+".human.txt"] = true
		want[scenarioID+".replay.canonical.json"] = true
	}
	entries, err := os.ReadDir(filepath.Join("testdata", "release_regression"))
	if err != nil {
		t.Fatalf("read release regression fixtures: %v", err)
	}
	got := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "README.md" || name == "matrix.json" {
			continue
		}
		got[name] = true
	}
	if !sameStringSlice(keys(got), keys(want)) {
		t.Fatalf("release regression fixture files mismatch\ngot:  %v\nwant: %v", keys(got), keys(want))
	}
}

func TestReleaseRegressionHumanFixturesUseCanonicalLF(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "release_regression", "*.human.txt"))
	if err != nil {
		t.Fatalf("glob release regression human fixtures: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no release regression human fixtures found")
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read release regression human fixture %s: %v", path, err)
		}
		if bytes.Contains(data, []byte{'\r'}) {
			t.Fatalf("release regression human fixture %s contains CR bytes; fixtures are reviewed as exact LF-only bytes", path)
		}
		if !bytes.HasSuffix(data, []byte{'\n'}) {
			t.Fatalf("release regression human fixture %s must end with LF", path)
		}
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

func TestReleaseRegressionRoutingPinVerifierDiversityIsHostIndependent(t *testing.T) {
	scenario := releaseRegressionScenario(t, "routing-pin-verifier-diversity")
	first, err := Execute(context.Background(), scenario, Options{})
	if err != nil {
		t.Fatalf("Execute first host: %v", err)
	}
	changedHost := scenario
	changedHost.Host = HostRuntime{
		ConductorHostID: "host-claude-conductor",
		AdapterID:       "host-claude",
		Capabilities: HostCapabilities{
			SupportsPush:   true,
			SupportsWake:   true,
			SupportsFollow: true,
			SupportsPoll:   true,
		},
	}
	second, err := Execute(context.Background(), changedHost, Options{})
	if err != nil {
		t.Fatalf("Execute changed host: %v", err)
	}
	worker := decisionByEvent(t, first.Decisions, "event-worker-route")
	verifier := decisionByEvent(t, first.Decisions, "event-verifier-route")
	if worker.UserPinnedModelID != "model-b" || worker.ChosenCandidateID != "model-b" {
		t.Fatalf("worker pin decision = %#v, want pinned model-b", worker)
	}
	if verifier.UserOverrideModelID != "model-verifier" || verifier.ChosenCandidateID != "model-verifier" {
		t.Fatalf("verifier override decision = %#v, want model-verifier", verifier)
	}
	if worker.ChosenCandidateID != decisionByEvent(t, second.Decisions, "event-worker-route").ChosenCandidateID ||
		verifier.ChosenCandidateID != decisionByEvent(t, second.Decisions, "event-verifier-route").ChosenCandidateID {
		t.Fatalf("host identity changed worker/verifier policy selection\nfirst=%#v\nsecond=%#v", first.Decisions, second.Decisions)
	}
	if worker.ChosenCandidateID == "model-worker-high-quota" || verifier.ChosenCandidateID == "model-worker-high-quota" {
		t.Fatalf("worker-only high quota model crossed role/pin boundary: %#v", first.Decisions)
	}
}

func TestReleaseRegressionQuotaResetTransitionUsesClockAndDoesNotFabricateUnknown(t *testing.T) {
	scenario := releaseRegressionScenario(t, "routing-churn-quota-reset-fallback")
	for _, quota := range scenario.Inventory.Quotas {
		if (quota.Confidence == providerinventory.ConfidenceUnknown || quota.Confidence == providerinventory.ConfidenceStale) && quota.RemainingValue != nil {
			t.Fatalf("quota %s confidence=%s carries fabricated remaining value %d", quota.QuotaSnapshotID, quota.Confidence, *quota.RemainingValue)
		}
	}
	result, err := Execute(context.Background(), scenario, Options{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	before := decisionByEvent(t, result.Decisions, "event-route-before-reset")
	after := decisionByEvent(t, result.Decisions, "event-route-after-reset")
	if before.ChosenCandidateID != "model-pre-reset" {
		t.Fatalf("before reset chose %q, want model-pre-reset: %#v", before.ChosenCandidateID, before)
	}
	if after.ChosenCandidateID != "model-zz" {
		t.Fatalf("after reset chose %q, want model-zz: %#v", after.ChosenCandidateID, after)
	}
	if !containsString(before.QuotaSnapshotIDs, "quota-e-pre-reset") || containsString(before.QuotaSnapshotIDs, "quota-e-reset") {
		t.Fatalf("before reset quota snapshots = %#v, want only pre-reset window active", before.QuotaSnapshotIDs)
	}
	if !containsString(after.QuotaSnapshotIDs, "quota-e-reset") || containsString(after.QuotaSnapshotIDs, "quota-e-pre-reset") {
		t.Fatalf("after reset quota snapshots = %#v, want reset window active", after.QuotaSnapshotIDs)
	}
}

func TestReleaseRegressionUnsupportedPushWakeTruthfulFollowPoll(t *testing.T) {
	scenario := releaseRegressionScenario(t, "unsupported-push-wake-truthful-follow")
	result, err := Execute(context.Background(), scenario, Options{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.DurableState.ProviderReceipts) != 0 {
		t.Fatalf("provider receipts = %#v, want no external provider calls", result.DurableState.ProviderReceipts)
	}
	if got := len(result.DurableState.HostDeliveries); got != 1 {
		t.Fatalf("host deliveries = %d, want 1", got)
	}
	delivery := result.DurableState.HostDeliveries[0]
	if delivery.Transport != "follow-poll" || !delivery.Durable || !delivery.FollowScheduled || !delivery.PollScheduled || delivery.PushAttempted || delivery.WakeAttempted || delivery.VisibilityClaimed || delivery.Acknowledged || delivery.ExternalCall {
		t.Fatalf("host delivery = %#v, want truthful durable follow/poll without push/wake/ack/visibility/external call", delivery)
	}
}

func TestReleaseRegressionFederationPartialAndOwnershipConflictEvidence(t *testing.T) {
	partialScenario := releaseRegressionScenario(t, "federation-cancellation-partial-result")
	partialResult, err := Execute(context.Background(), partialScenario, Options{})
	if err != nil {
		t.Fatalf("Execute partial scenario: %v", err)
	}
	if got := len(partialResult.DurableState.PartialResults); got != 1 {
		t.Fatalf("partial results = %d, want 1", got)
	}
	partial := partialResult.DurableState.PartialResults[0]
	if partial.Status != "cancelled-partial" || !partial.Durable || partial.ExternalCall || partial.ParentTaskID != "task-a" {
		t.Fatalf("partial result = %#v, want durable cancelled partial under task-a with no external call", partial)
	}

	conflictScenario := releaseRegressionScenario(t, "federation-ownership-conflict")
	conflictResult, err := Execute(context.Background(), conflictScenario, Options{})
	if err != nil {
		t.Fatalf("Execute conflict scenario: %v", err)
	}
	if got := len(conflictResult.DurableState.AgentOwners); got != 1 {
		t.Fatalf("agent owners = %d, want one retained owner", got)
	}
	if got := len(conflictResult.DurableState.OwnerConflicts); got != 1 {
		t.Fatalf("owner conflicts = %d, want 1", got)
	}
	conflict := conflictResult.DurableState.OwnerConflicts[0]
	if conflict.ResourceKey != "resource-a" || conflict.ExistingOwnerTask != "task-a" || conflict.ConflictState != "rejected-existing-owner" || conflict.ExternalCall {
		t.Fatalf("owner conflict = %#v, want rejected conflict against task-a without external call", conflict)
	}
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
		"routing-churn-quota-reset-fallback":     routingChurnQuotaResetFallbackScenario,
		"routing-pin-verifier-diversity":         routingPinVerifierDiversityScenario,
		"handoff-crash-replay-idempotent":        handoffCrashReplayIdempotentScenario,
		"federation-nested-bounds-concurrency":   federationNestedBoundsConcurrencyScenario,
		"federation-cancellation-partial-result": federationCancellationPartialResultScenario,
		"federation-ownership-conflict":          federationOwnershipConflictScenario,
		"fallback-rate-limit-outage-bounded":     fallbackRateLimitOutageBoundedScenario,
		"unsupported-push-wake-truthful-follow":  unsupportedPushWakeTruthfulFollowScenario,
	}
}

func routingChurnQuotaResetFallbackScenario() Scenario {
	scenario := releaseRegressionBase("routing-churn-quota-reset-fallback")
	zero := int64(0)
	preResetRemaining := int64(5)
	resetRemaining := int64(25)
	scenario.Inventory.Models[0].Availability = providerinventory.AvailabilityTemporarilyUnavailable
	scenario.Inventory.Quotas[1].Confidence = providerinventory.ConfidenceUnknown
	scenario.Inventory.Quotas[1].RemainingValue = nil
	scenario.Inventory.Models = append(scenario.Inventory.Models,
		releaseModel("model-c", "adapter-a", "account-a", "model-stale", providerinventory.CatalogRoleWorker),
		releaseModel("model-d", "adapter-a", "account-a", "model-exhausted", providerinventory.CatalogRoleWorker),
		releaseModel("model-pre-reset", "adapter-a", "account-a", "model-pre-reset", providerinventory.CatalogRoleWorker),
		releaseModel("model-zz", "adapter-a", "account-a", "model-reset", providerinventory.CatalogRoleWorker),
	)
	scenario.Inventory.Quotas = append(scenario.Inventory.Quotas,
		releaseQuota("quota-c-stale", "adapter-a", "account-a", "model-c", providerinventory.ConfidenceStale, nil),
		releaseQuota("quota-d-exhausted", "adapter-a", "account-a", "model-d", providerinventory.ConfidenceExact, &zero),
		releaseQuota("quota-e-pre-reset", "adapter-a", "account-a", "model-pre-reset", providerinventory.ConfidenceExact, &preResetRemaining),
		releaseQuota("quota-e-reset", "adapter-a", "account-a", "model-zz", providerinventory.ConfidenceExact, &resetRemaining),
	)
	scenario.Inventory.Quotas[len(scenario.Inventory.Quotas)-2].WindowStart = "2026-07-14T00:00:00Z"
	scenario.Inventory.Quotas[len(scenario.Inventory.Quotas)-2].WindowEnd = "2026-07-14T00:00:00.01Z"
	scenario.Inventory.Quotas[len(scenario.Inventory.Quotas)-1].WindowStart = "2026-07-14T00:00:00.01Z"
	scenario.Inventory.Quotas[len(scenario.Inventory.Quotas)-1].WindowEnd = "2026-07-15T00:00:00.01Z"
	scenario.Tasks[0].CandidateModelIDs = []string{"model-a", "model-b", "model-c", "model-d", "model-pre-reset", "model-zz"}
	scenario.InjectedEvents = []InjectedEvent{
		{EventID: "event-route-before-reset", Kind: "route_task", TaskID: "task-a", ConcurrencyGroup: "ready"},
		{EventID: "event-route-after-reset", Kind: "route_task", TaskID: "task-a", ConcurrencyGroup: "ready"},
	}
	scenario.ConcurrencyScript = []ConcurrencyOrder{{Group: "ready", EventIDs: []string{"event-route-before-reset", "event-route-after-reset"}}}
	scenario.Invariants = []Invariant{
		{InvariantID: "inv-route-reset-fallback", Kind: "route_accepted", TaskID: "task-a"},
		{InvariantID: "inv-route-reset-candidate", Kind: "chosen_candidate", TaskID: "task-a", Expected: "model-zz"},
	}
	return scenario
}

func routingPinVerifierDiversityScenario() Scenario {
	scenario := releaseRegressionBase("routing-pin-verifier-diversity")
	remaining := int64(10_000)
	scenario.Host = HostRuntime{
		ConductorHostID: "host-codex-conductor",
		AdapterID:       "host-codex",
		Capabilities: HostCapabilities{
			SupportsFollow: true,
			SupportsPoll:   true,
		},
	}
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
	scenario.Inventory.Models = append(scenario.Inventory.Models,
		releaseModel("model-worker-high-quota", "adapter-a", "account-a", "model-worker-high-quota", providerinventory.CatalogRoleWorker),
		releaseModel("model-verifier", "adapter-verifier", "account-verifier", "model-verifier", providerinventory.CatalogRoleVerifier),
	)
	scenario.Inventory.Quotas = append(scenario.Inventory.Quotas,
		releaseQuota("quota-worker-high", "adapter-a", "account-a", "model-worker-high-quota", providerinventory.ConfidenceExact, &remaining),
		releaseQuota("quota-verifier", "adapter-verifier", "account-verifier", "model-verifier", providerinventory.ConfidenceExact, &remaining),
	)
	scenario.Tasks[0].UserPinnedModelID = "model-b"
	scenario.Tasks[0].CandidateModelIDs = []string{"model-a", "model-b", "model-worker-high-quota"}
	verifier := releaseTask("task-verifier", "Verifier", 1, "resource-verifier")
	verifier.Role = providerinventory.CatalogRoleVerifier
	verifier.UserOverrideModelID = "model-verifier"
	verifier.CandidateModelIDs = []string{"model-worker-high-quota", "model-verifier"}
	scenario.Tasks = append(scenario.Tasks, verifier)
	scenario.InjectedEvents = []InjectedEvent{
		{EventID: "event-worker-route", Kind: "route_task", TaskID: "task-a", ConcurrencyGroup: "ready"},
		{EventID: "event-verifier-route", Kind: "route_task", TaskID: "task-verifier", ConcurrencyGroup: "ready"},
	}
	scenario.ConcurrencyScript = []ConcurrencyOrder{{Group: "ready", EventIDs: []string{"event-worker-route", "event-verifier-route"}}}
	scenario.Invariants = []Invariant{
		{InvariantID: "inv-worker-pin-route", Kind: "chosen_candidate", TaskID: "task-a", Expected: "model-b"},
		{InvariantID: "inv-verifier-override-route", Kind: "chosen_candidate", TaskID: "task-verifier", Expected: "model-verifier"},
	}
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

func federationCancellationPartialResultScenario() Scenario {
	scenario := releaseRegressionBase("federation-cancellation-partial-result")
	scenario.Tasks = append(scenario.Tasks, releaseTask("task-child", "Child", 2, "resource-child"))
	scenario.InjectedEvents = []InjectedEvent{{
		EventID:          "event-child-partial",
		Kind:             "partial_result",
		TaskID:           "task-child",
		ConcurrencyGroup: "partial",
		PayloadRef:       "task-a",
	}}
	scenario.ConcurrencyScript = []ConcurrencyOrder{{Group: "partial", EventIDs: []string{"event-child-partial"}}}
	scenario.Invariants = []Invariant{{
		InvariantID: "inv-child-cancelled-partial",
		Kind:        "partial_result_recorded",
		TaskID:      "task-child",
		Expected:    "cancelled-partial",
	}}
	return scenario
}

func federationOwnershipConflictScenario() Scenario {
	scenario := releaseRegressionBase("federation-ownership-conflict")
	scenario.Tasks[1].OwnerResourceKey = "resource-a"
	scenario.InjectedEvents = []InjectedEvent{
		{EventID: "event-own-a", Kind: "agent_own", TaskID: "task-a", ConcurrencyGroup: "ownership"},
		{EventID: "event-conflict-b", Kind: "ownership_conflict", TaskID: "task-b", ConcurrencyGroup: "ownership", PayloadRef: "resource-a"},
	}
	scenario.ConcurrencyScript = []ConcurrencyOrder{{Group: "ownership", EventIDs: []string{"event-own-a", "event-conflict-b"}}}
	scenario.Invariants = []Invariant{
		{InvariantID: "inv-one-owner", Kind: "one_owner_per_resource"},
		{InvariantID: "inv-owner-conflict", Kind: "ownership_conflict_recorded", TaskID: "task-b", Expected: "rejected-existing-owner"},
	}
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
	scenario.Host = HostRuntime{
		ConductorHostID: "host-without-push-wake",
		AdapterID:       "host-local",
		Capabilities: HostCapabilities{
			SupportsPush:   false,
			SupportsWake:   false,
			SupportsFollow: true,
			SupportsPoll:   true,
		},
	}
	scenario.InjectedEvents = []InjectedEvent{{
		EventID:          "event-follow-poll-delivery",
		Kind:             "host_deliver",
		TaskID:           "task-a",
		ConcurrencyGroup: "follow",
		PayloadRef:       "release-status-poll-cursor",
	}}
	scenario.ConcurrencyScript = []ConcurrencyOrder{{Group: "follow", EventIDs: []string{"event-follow-poll-delivery"}}}
	scenario.Invariants = []Invariant{
		{InvariantID: "inv-follow-route", Kind: "route_accepted", TaskID: "task-a"},
		{InvariantID: "inv-truthful-follow-poll", Kind: "host_truthful_follow_poll", TaskID: "task-a"},
		{InvariantID: "inv-no-provider-calls", Kind: "no_provider_receipts"},
	}
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
		if existing, err := os.ReadFile(path); err == nil {
			existingMeta := releaseSnapshotMetadata(t, name, existing)
			gotMeta := releaseSnapshotMetadata(t, name, got)
			if existingMeta == gotMeta {
				t.Fatalf("refusing to update snapshot %s with unchanged fixture/policy versions %q; bump reviewed release regression metadata before rewriting evidence", path, gotMeta)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read existing snapshot %s: %v", path, err)
		}
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

func releaseSnapshotMetadata(t *testing.T, name string, data []byte) string {
	t.Helper()
	if strings.HasSuffix(name, ".json") {
		var result Result
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatalf("decode snapshot metadata %s: %v", name, err)
		}
		return result.PolicyProvenance.PolicyVersion + "|" + releaseExtensionMetadata(t, result.Extensions)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "policy: version=") {
			fields := strings.Fields(line)
			if len(fields) > 1 {
				return strings.TrimPrefix(fields[1], "version=")
			}
		}
	}
	t.Fatalf("snapshot %s has no release metadata", name)
	return ""
}

func releaseExtensionMetadata(t *testing.T, extensions map[string]json.RawMessage) string {
	t.Helper()
	raw := extensions["x_release_regression"]
	if len(raw) == 0 {
		t.Fatalf("release snapshot missing x_release_regression metadata")
	}
	var metadata struct {
		FixtureSchemaVersion string `json:"fixture_schema_version"`
		PolicySchemaVersion  string `json:"policy_schema_version"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("decode x_release_regression metadata: %v", err)
	}
	return metadata.FixtureSchemaVersion + "|" + metadata.PolicySchemaVersion
}

func assertNoDuplicateReleaseSideEffects(t *testing.T, state DurableState) {
	t.Helper()
	assertUniqueBy(t, "provider receipt event", state.ProviderReceipts, func(value ProviderReceipt) string {
		return value.EventID + "\x00" + value.TaskID + "\x00" + value.ModelCapabilityID
	})
	assertUniqueBy(t, "host delivery event", state.HostDeliveries, func(value HostDelivery) string {
		return value.EventID + "\x00" + value.TaskID + "\x00" + value.HostID + "\x00" + value.PayloadRef
	})
	assertUniqueBy(t, "partial result event", state.PartialResults, func(value PartialResult) string {
		return value.EventID + "\x00" + value.TaskID + "\x00" + value.ParentTaskID
	})
	assertUniqueBy(t, "owner conflict event", state.OwnerConflicts, func(value OwnerConflict) string {
		return value.EventID + "\x00" + value.TaskID + "\x00" + value.ResourceKey + "\x00" + value.ExistingOwnerTask
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

func decisionByEvent(t *testing.T, decisions []DecisionRecord, eventID string) DecisionRecord {
	t.Helper()
	for _, decision := range decisions {
		if decision.EventID == eventID {
			return decision
		}
	}
	t.Fatalf("missing decision for event %s in %#v", eventID, decisions)
	return DecisionRecord{}
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

func keysFuncMap(values map[string]func() Scenario) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
