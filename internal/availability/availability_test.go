package availability

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
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
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "state.db"), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()
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

func onlyScore(t *testing.T, result Result) Score {
	t.Helper()
	if len(result.Scores) != 1 {
		t.Fatalf("scores = %d, want 1: %#v", len(result.Scores), result.Scores)
	}
	return result.Scores[0]
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
