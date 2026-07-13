package availability

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestHardModelConstraintCannotBeOverriddenByHighScore(t *testing.T) {
	now := fixedNow()
	inventory := healthyInventory(now)
	inventory.ModelCapabilities[0].AvailabilityState = providerinventory.AvailabilityRemoved
	inventory.ModelCapabilities[0].LifecycleState = providerinventory.LifecycleRemoved

	result := Derive(Inputs{
		Inventory: inventory,
		Policy:    Policy{ExactQuotaRequired: true},
		Now:       now,
	})
	score := onlyScore(t, result)
	if score.Score != 100 {
		t.Fatalf("score = %d, want high diagnostic score from non-model components", score.Score)
	}
	if score.Eligible {
		t.Fatalf("eligible = true, want hard model failure to block")
	}
	if !hasReason(score.HardIneligibleReasons, ReasonModelUnavailable) {
		t.Fatalf("hard reasons = %#v, want model unavailable", score.HardIneligibleReasons)
	}
	var human bytes.Buffer
	if err := RenderScore(&human, score); err != nil {
		t.Fatalf("RenderScore: %v", err)
	}
	for _, want := range []string{"hard:", "model-unavailable", "quota:", "auth:"} {
		if !strings.Contains(human.String(), want) {
			t.Fatalf("human output missing %q:\n%s", want, human.String())
		}
	}
}

func TestStaleAndUnknownTelemetryRemainExplicit(t *testing.T) {
	now := fixedNow()
	inventory := healthyInventory(now)
	inventory.AuthReadiness = nil
	inventory.QuotaSnapshots[0].Confidence = providerinventory.ConfidenceStale
	inventory.QuotaSnapshots[0].FreshnessState = providerinventory.FreshnessStale
	inventory.QuotaSnapshots[0].StaleAfter = now.Add(-time.Minute).Format(time.RFC3339Nano)

	score := onlyScore(t, Derive(Inputs{
		Inventory: inventory,
		Policy:    Policy{ExactQuotaRequired: true},
		Now:       now,
	}))
	if score.Eligible {
		t.Fatal("eligible = true, want stale/unknown hard policy refusal")
	}
	for _, want := range []ReasonCode{ReasonUnknownTelemetry, ReasonStaleEvidence, ReasonQuotaConfidenceInsufficient} {
		if ReasonCount(score, want) == 0 {
			t.Fatalf("score missing reason %s: %#v", want, score)
		}
	}
	if score.ScoreConfidence != providerinventory.ConfidenceStale {
		t.Fatalf("score confidence = %s, want stale", score.ScoreConfidence)
	}
}

func TestIdenticalEvidenceProducesIdenticalObservationAndScoreIDs(t *testing.T) {
	now := fixedNow()
	inputs := Inputs{Inventory: healthyInventory(now), Policy: Policy{ExactQuotaRequired: true}, Now: now}
	first := Derive(inputs)
	second := Derive(inputs)
	if !reflect.DeepEqual(first, second) {
		left, _ := json.MarshalIndent(first, "", "  ")
		right, _ := json.MarshalIndent(second, "", "  ")
		t.Fatalf("derive not deterministic:\n%s\n---\n%s", left, right)
	}
	if DebugFingerprint(first) != DebugFingerprint(second) {
		t.Fatalf("fingerprints differ: %s != %s", DebugFingerprint(first), DebugFingerprint(second))
	}
}

func TestFailureAndPartialDataFixtures(t *testing.T) {
	now := fixedNow()
	partial := healthyInventory(now)
	partial.ProbeResults[0].Outcome = providerinventory.OutcomeProbeFailed
	partial.ProbeResults[0].TimedOut = true
	partial.ProbeResults[0].Confidence = providerinventory.ConfidenceUnknown
	partial.ProbeResults[0].GapReasons = []string{"transport timeout"}
	partial.QuotaSnapshots[0].TerminalErrorCode = "ErrRateLimited"
	partial.QuotaSnapshots[0].GapReasons = []string{"HTTP 429"}

	result := Derive(Inputs{Inventory: partial, Policy: Policy{ExactQuotaRequired: true}, Now: now})
	score := onlyScore(t, result)
	for _, want := range []ReasonCode{ReasonTransport, ReasonRateLimited429} {
		if ReasonCount(score, want) == 0 {
			t.Fatalf("score missing reason %s: %#v", want, score)
		}
	}
	kinds := map[ObservationKind]bool{}
	for _, observation := range result.Observations {
		kinds[observation.ObservationKind] = true
	}
	for _, want := range []ObservationKind{ObservationTransportFailure, ObservationRateLimited} {
		if !kinds[want] {
			t.Fatalf("observations missing %s: %#v", want, result.Observations)
		}
	}
}

func TestPersistLoadRoundTripIncludesEvidenceIDs(t *testing.T) {
	ctx := context.Background()
	now := fixedNow()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := storage.Open(ctx, storage.Options{Path: dbPath, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()
	peerStore, err := storage.Open(ctx, storage.Options{Path: dbPath, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("peer storage.Open: %v", err)
	}
	defer peerStore.Close()
	result := Derive(Inputs{Inventory: healthyInventory(now), Policy: Policy{ExactQuotaRequired: true}, Now: now})
	if err := Persist(ctx, store, result); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	loaded, err := Load(ctx, store)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	score := onlyScore(t, loaded)
	for _, bucket := range [][]string{score.EvidenceRecordIDs, score.ObservationIDs, score.QuotaSnapshotIDs} {
		if len(bucket) == 0 {
			t.Fatalf("score missing evidence bucket: %#v", score)
		}
	}
}

func TestNormalizeReasonCodes(t *testing.T) {
	cases := map[string]ReasonCode{
		"HTTP 429 Too Many Requests": ReasonRateLimited429,
		"auth expired":               ReasonAuth,
		"model unavailable":          ReasonModelUnavailable,
		"transport timeout":          ReasonTransport,
		"provider 5xx outage":        ReasonProviderOutage,
		"malformed JSON response":    ReasonMalformedResponse,
		"stale quota cache":          ReasonStaleEvidence,
		"quota exhausted":            ReasonQuotaExhausted,
	}
	for input, want := range cases {
		if got := NormalizeReasonCode(input); got != want {
			t.Fatalf("NormalizeReasonCode(%q) = %s, want %s", input, got, want)
		}
	}
}

func TestBreakerTransitionsAreScopedByErrorClass(t *testing.T) {
	now := fixedNow()
	scope := Scope{
		ProjectID:              "proj_availability",
		AdapterID:              "codex",
		ProviderInstallationID: "pinst_codex",
		AccountProfileID:       "acct_codex",
		ModelCapabilityID:      "mcap_codex_gpt55",
		CanonicalModelID:       "gpt-5.5",
	}
	observations := []Observation{
		testObservation(ObservationRateLimited, scope, now, ReasonRateLimited429, "rate"),
		testObservation(ObservationQuotaExhausted, scope, now.Add(time.Second), ReasonQuotaExhausted, "quota"),
		testObservation(ObservationAuthFailure, scope, now.Add(2*time.Second), ReasonAuth, "auth"),
		testObservation(ObservationModelUnavailable, scope, now.Add(3*time.Second), ReasonModelUnavailable, "model"),
		testObservation(ObservationTransportFailure, scope, now.Add(4*time.Second), ReasonTransport, "transport-1"),
		testObservation(ObservationTransportFailure, scope, now.Add(5*time.Second), ReasonTransport, "transport-2"),
	}
	result := Derive(Inputs{
		Observations: observations,
		Policy: Policy{
			FailureThreshold:     2,
			BaseCooldown:         time.Minute,
			HalfOpenProbeBudget:  1,
			RequiredSuccessCount: 1,
		},
		Now: now,
	})
	byKind := map[BreakerKind]CircuitBreaker{}
	for _, breaker := range result.CircuitBreakers {
		byKind[breaker.BreakerKind] = breaker
	}
	for _, kind := range []BreakerKind{BreakerRateLimit, BreakerQuota, BreakerAuth, BreakerModel, BreakerTransport} {
		breaker, ok := byKind[kind]
		if !ok {
			t.Fatalf("missing breaker kind %s: %#v", kind, result.CircuitBreakers)
		}
		if breaker.State != BreakerOpen {
			t.Fatalf("%s state = %s, want open", kind, breaker.State)
		}
		if breaker.OpenUntil == "" {
			t.Fatalf("%s open_until missing: %#v", kind, breaker)
		}
	}
	if byKind[BreakerAuth].Scope.ModelCapabilityID != "" {
		t.Fatalf("auth breaker scope includes model: %#v", byKind[BreakerAuth].Scope)
	}
	if byKind[BreakerRateLimit].Scope.ModelCapabilityID == "" || byKind[BreakerQuota].Scope.AccountProfileID == "" {
		t.Fatalf("provider/account/model scoped breakers lost scope: rate=%#v quota=%#v", byKind[BreakerRateLimit].Scope, byKind[BreakerQuota].Scope)
	}
}

func TestGrokQuotaObservationCannotOpenProviderWideBreaker(t *testing.T) {
	now := fixedNow()
	installationID := "pinst_grok"
	accountID := "acct_grok"
	modelID := "mcap_grok_45"
	remaining := int64(0)
	inventory := providerinventory.Report{
		SchemaVersion: providerinventory.ProviderInventoryJSONSchema,
		GeneratedAt:   now.Format(time.RFC3339Nano),
		Confidence:    providerinventory.ConfidenceExact,
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			SchemaVersion:          providerinventory.QuotaSnapshotSchema,
			RecordVersion:          1,
			QuotaSnapshotID:        "qsnap_grok_rate_limited",
			QuotaSourceID:          "qsrc_grok_acp_billing",
			SourceKind:             providerinventory.QuotaSourceOfficialCLICommand,
			AdapterID:              "grok",
			ProviderInstallationID: &installationID,
			AccountProfileID:       &accountID,
			ModelCapabilityID:      &modelID,
			ScopeKey:               "provider:grok/account:acct_grok/model:grok-4.5/detail:rate_limit_remaining",
			QuantityKind:           providerinventory.QuantityProviderDefined,
			ProviderQuantityName:   "rate_limit_remaining",
			Unit:                   "request",
			WindowKind:             providerinventory.WindowUnknown,
			ResetSemantics:         providerinventory.ResetUnknown,
			RemainingValue:         &remaining,
			ValueScale:             0,
			Confidence:             providerinventory.ConfidenceExact,
			FreshnessState:         providerinventory.FreshnessFresh,
			CapturedAt:             now.Format(time.RFC3339Nano),
			StaleAfter:             now.Add(time.Hour).Format(time.RFC3339Nano),
			GapReasons:             []string{"rate-limited-429"},
			TerminalErrorCode:      "ErrRateLimited",
		}},
	}

	result := Derive(Inputs{
		Inventory: inventory,
		Policy:    Policy{BaseCooldown: time.Minute},
		Now:       now,
	})
	if len(result.CircuitBreakers) != 1 {
		t.Fatalf("breakers = %#v, want one scoped rate-limit breaker", result.CircuitBreakers)
	}
	breaker := result.CircuitBreakers[0]
	if breaker.BreakerKind != BreakerRateLimit || breaker.State != BreakerOpen {
		t.Fatalf("breaker = %#v, want open rate-limit breaker", breaker)
	}
	if breaker.Scope.AdapterID != "grok" || breaker.Scope.ProviderInstallationID != installationID || breaker.Scope.AccountProfileID != accountID || breaker.Scope.ModelCapabilityID != modelID {
		t.Fatalf("breaker scope widened or lost dimensions: %#v", breaker.Scope)
	}
	if breaker.Scope.AccountProfileID == "" || breaker.Scope.ModelCapabilityID == "" {
		t.Fatalf("breaker scope is provider-wide: %#v", breaker.Scope)
	}
}

func TestBreakerReplayDoesNotExtendCooldown(t *testing.T) {
	now := fixedNow()
	scope := Scope{AdapterID: "codex", AccountProfileID: "acct_codex", ModelCapabilityID: "mcap_codex_gpt55"}
	observation := testObservation(ObservationRateLimited, scope, now, ReasonRateLimited429, "rate")
	observation.RetryAfter = now.Add(10 * time.Minute).Format(time.RFC3339Nano)
	first := Derive(Inputs{Observations: []Observation{observation}, Policy: Policy{BaseCooldown: time.Minute}, Now: now})
	if len(first.CircuitBreakers) != 1 {
		t.Fatalf("breakers = %#v, want one", first.CircuitBreakers)
	}
	openUntil := first.CircuitBreakers[0].OpenUntil
	recordVersion := first.CircuitBreakers[0].RecordVersion
	replayed := Derive(Inputs{
		Observations:    []Observation{observation},
		CircuitBreakers: first.CircuitBreakers,
		Policy:          Policy{BaseCooldown: time.Minute},
		Now:             now.Add(time.Minute),
	})
	if got := replayed.CircuitBreakers[0].OpenUntil; got != openUntil {
		t.Fatalf("open_until = %s, want replay-stable %s", got, openUntil)
	}
	if got := replayed.CircuitBreakers[0].RecordVersion; got != recordVersion {
		t.Fatalf("record_version = %d, want unchanged %d", got, recordVersion)
	}
}

func TestHalfOpenProbeLeaseCoalescesConcurrentRecovery(t *testing.T) {
	ctx := context.Background()
	now := fixedNow()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := storage.Open(ctx, storage.Options{Path: dbPath, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()
	peerStore, err := storage.Open(ctx, storage.Options{Path: dbPath, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("peer storage.Open: %v", err)
	}
	defer peerStore.Close()
	scope := Scope{AdapterID: "codex", AccountProfileID: "acct_codex", ModelCapabilityID: "mcap_codex_gpt55"}
	observation := testObservation(ObservationRateLimited, scope, now.Add(-time.Hour), ReasonRateLimited429, "rate")
	observation.RetryAfter = now.Add(-time.Second).Format(time.RFC3339Nano)
	opened := Derive(Inputs{
		Observations: []Observation{observation},
		Policy:       Policy{BaseCooldown: time.Minute, HalfOpenProbeBudget: 1},
		Now:          now.Add(-time.Hour),
	})
	halfOpen := Derive(Inputs{
		Observations:    []Observation{observation},
		CircuitBreakers: opened.CircuitBreakers,
		Policy:          Policy{BaseCooldown: time.Minute, HalfOpenProbeBudget: 1},
		Now:             now,
	})
	if err := Persist(ctx, store, halfOpen); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	breaker := halfOpen.CircuitBreakers[0]
	if breaker.State != BreakerHalfOpen {
		t.Fatalf("state = %s, want half-open", breaker.State)
	}

	var acquired int32
	var wg sync.WaitGroup
	stores := []storage.Store{store, peerStore}
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lease, err := AcquireHalfOpenProbeLease(ctx, stores[i%len(stores)], breaker.CircuitBreakerID, string(rune('a'+i)), Policy{ProbeLeaseDuration: time.Minute, HalfOpenProbeBudget: 1}, now)
			if err != nil {
				t.Errorf("AcquireHalfOpenProbeLease: %v", err)
				return
			}
			if lease.Acquired {
				atomic.AddInt32(&acquired, 1)
			}
		}(i)
	}
	wg.Wait()
	if acquired != 1 {
		t.Fatalf("acquired leases = %d, want exactly 1", acquired)
	}
	loaded, err := Load(ctx, store)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.CircuitBreakers) != 1 || loaded.CircuitBreakers[0].HalfOpenProbeCount != 1 || loaded.CircuitBreakers[0].ProbeLeaseOwner == "" {
		t.Fatalf("loaded breaker lease state = %#v", loaded.CircuitBreakers)
	}
}

func onlyScore(t *testing.T, result Result) Score {
	t.Helper()
	if len(result.Scores) != 1 {
		t.Fatalf("scores = %d, want 1: %#v", len(result.Scores), result.Scores)
	}
	return result.Scores[0]
}

func testObservation(kind ObservationKind, scope Scope, observedAt time.Time, reason ReasonCode, source string) Observation {
	return normalizeObservation(Observation{
		ObservationKind: kind,
		Scope:           scope,
		SourceRecordIDs: []string{"src_" + source},
		ObservedAt:      observedAt.UTC().Format(time.RFC3339Nano),
		FailureClass:    reason,
		Confidence:      providerinventory.ConfidenceExact,
		ReasonCodes:     []ReasonCode{reason},
	})
}

func fixedNow() time.Time {
	return time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
}

func healthyInventory(now time.Time) providerinventory.Report {
	projectID := "proj_availability"
	installationID := "pinst_codex"
	accountID := "acct_codex"
	modelID := "mcap_codex_gpt55"
	remaining := int64(100)
	capturedAt := now.Format(time.RFC3339Nano)
	staleAfter := now.Add(time.Hour).Format(time.RFC3339Nano)
	return providerinventory.Report{
		SchemaVersion: providerinventory.ProviderInventoryJSONSchema,
		GeneratedAt:   capturedAt,
		Confidence:    providerinventory.ConfidenceExact,
		Installations: []providerinventory.ProviderInstallation{{
			SchemaVersion:          providerinventory.ProviderInstallationSchema,
			RecordVersion:          1,
			ProjectID:              &projectID,
			ProviderInstallationID: installationID,
			AdapterID:              "codex",
			InstallationState:      providerinventory.InstallationInstalled,
			UsableForInvocation:    "yes",
			CapturedAt:             capturedAt,
			StaleAfter:             staleAfter,
			FreshnessState:         providerinventory.FreshnessFresh,
			Confidence:             providerinventory.ConfidenceExact,
		}},
		ProbeResults: []providerinventory.ProbeResult{{
			SchemaVersion:          providerinventory.ProbeResultSchema,
			RecordVersion:          1,
			ProjectID:              &projectID,
			ProbeResultID:          "probe_codex_version",
			AdapterID:              "codex",
			ProviderInstallationID: &installationID,
			Outcome:                providerinventory.OutcomeInstalled,
			NetworkPermission:      providerinventory.NetworkNotNeeded,
			CapturedAt:             capturedAt,
			StaleAfter:             staleAfter,
			FreshnessState:         providerinventory.FreshnessFresh,
			Confidence:             providerinventory.ConfidenceExact,
		}},
		AuthReadiness: []providerinventory.AuthReadiness{{
			SchemaVersion:          providerinventory.AuthReadinessSchema,
			RecordVersion:          1,
			ProjectID:              &projectID,
			AuthReadinessID:        "auth_codex_ready",
			AdapterID:              "codex",
			ProviderInstallationID: &installationID,
			AccountProfileID:       &accountID,
			ReadinessState:         providerinventory.ReadinessReady,
			ReadinessConfidence:    providerinventory.ConfidenceExact,
			CapturedAt:             capturedAt,
			FreshnessState:         providerinventory.FreshnessFresh,
			Confidence:             providerinventory.ConfidenceExact,
		}},
		ModelCapabilities: []providerinventory.ModelCapability{{
			SchemaVersion:          providerinventory.ModelCapabilitySchema,
			RecordVersion:          1,
			ModelCapabilityID:      modelID,
			ModelCatalogSnapshotID: "mcats_codex",
			AdapterID:              "codex",
			CanonicalModelID:       "gpt-5.5",
			LifecycleState:         providerinventory.LifecycleAvailable,
			AvailabilityState:      providerinventory.AvailabilityAvailable,
			Aliases:                []providerinventory.ModelAlias{},
			RolesSupported:         []providerinventory.CatalogRole{providerinventory.CatalogRoleWorker},
			EntrySources:           []providerinventory.CatalogEntrySource{},
			CapturedAt:             capturedAt,
			StaleAfter:             staleAfter,
			FreshnessState:         providerinventory.FreshnessFresh,
			Confidence:             providerinventory.ConfidenceExact,
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			SchemaVersion:          providerinventory.QuotaSnapshotSchema,
			RecordVersion:          1,
			QuotaSnapshotID:        "qsnap_codex_exact",
			QuotaSourceID:          "qsrc_codex_fixture",
			SourceKind:             providerinventory.QuotaSourceFixture,
			AdapterID:              "codex",
			ProviderInstallationID: &installationID,
			AccountProfileID:       &accountID,
			ModelCapabilityID:      &modelID,
			ScopeKey:               `{"adapter_id":"codex"}`,
			QuantityKind:           providerinventory.QuantityRequests,
			Unit:                   "request",
			WindowKind:             providerinventory.WindowFixedHour,
			ResetSemantics:         providerinventory.ResetWindowBoundary,
			RemainingValue:         &remaining,
			ValueScale:             0,
			Confidence:             providerinventory.ConfidenceExact,
			FreshnessState:         providerinventory.FreshnessFresh,
			CapturedAt:             capturedAt,
			StaleAfter:             staleAfter,
		}},
	}
}
