package providerinventory

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestQuotaAllowlistedSourceRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedInventoryNow})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	source := fixtureQuotaSource("codex", QuotaSourceFixture, false)
	limit := int64(1000)
	used := int64(250)
	remaining := int64(750)
	now := fixedInventoryNow()
	capturedAt := now.Add(5 * time.Minute)
	windowEnd := now.Add(time.Hour)
	snapshot := normalizeQuotaSnapshot(QuotaSnapshot{
		QuotaSnapshotID:     quotaSnapshotID("codex", source.QuotaSourceID, "fixed-hour"),
		QuotaSourceID:       source.QuotaSourceID,
		SourceKind:          source.SourceKind,
		AdapterID:           "codex",
		ScopeKey:            "provider:codex/account:acct_fixture/model:gpt-fixture",
		QuantityKind:        QuantityRequests,
		Unit:                "request",
		WindowKind:          WindowFixedHour,
		WindowStart:         formatTime(now),
		WindowEnd:           formatTime(windowEnd),
		ResetAt:             formatTime(windowEnd),
		ResetSemantics:      ResetWindowBoundary,
		LimitValue:          &limit,
		UsedValue:           &used,
		RemainingValue:      &remaining,
		ValueScale:          0,
		Confidence:          ConfidenceExact,
		FieldConfidences:    map[string]Confidence{"limit_value": ConfidenceExact, "used_value": ConfidenceExact, "remaining_value": ConfidenceExact, "reset_at": ConfidenceExact},
		FreshnessState:      FreshnessFresh,
		CapturedAt:          formatTime(capturedAt),
		ValidUntil:          formatTime(windowEnd),
		StaleAfter:          formatTime(windowEnd),
		RawSourceHash:       rawSourceHash([]byte(`{"quota":{"limit":1000}}`)),
		RedactedDiagnostics: "fixture quota json parsed",
		ConflictSet:         []string{},
		GapReasons:          []string{},
		CreatedAt:           formatTime(capturedAt),
		UpdatedAt:           formatTime(capturedAt),
	})
	if err := ValidateQuotaTelemetrySource(source); err != nil {
		t.Fatalf("ValidateQuotaTelemetrySource: %v", err)
	}
	if err := ValidateQuotaSnapshot(source, snapshot); err != nil {
		t.Fatalf("ValidateQuotaSnapshot: %v", err)
	}
	report := Report{
		SchemaVersion:         ProviderInventoryJSONSchema,
		GeneratedAt:           formatTime(capturedAt),
		Confidence:            ConfidenceExact,
		Installations:         []ProviderInstallation{},
		ProbeResults:          []ProbeResult{},
		AccountProfiles:       []AccountProfile{},
		AuthReadiness:         []AuthReadiness{},
		ModelCatalogSnapshots: []ModelCatalogSnapshot{},
		ModelCapabilities:     []ModelCapability{},
		QuotaTelemetrySources: []QuotaTelemetrySource{source},
		QuotaSnapshots:        []QuotaSnapshot{snapshot},
		GapReasons:            []string{},
	}
	if err := Refresh(ctx, store, report, fixedInventoryNow()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	loaded, err := Load(ctx, store)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.QuotaTelemetrySources) != 1 || len(loaded.QuotaSnapshots) != 1 {
		t.Fatalf("loaded quota records = sources:%d snapshots:%d", len(loaded.QuotaTelemetrySources), len(loaded.QuotaSnapshots))
	}
	got := loaded.QuotaSnapshots[0]
	if got.Confidence != ConfidenceExact || got.Unit != "request" || got.ResetSemantics != ResetWindowBoundary || got.ScopeKey == "" {
		t.Fatalf("loaded quota snapshot lost contract fields: %#v", got)
	}
}

func TestQuotaLoadFreshnessUsesInjectedClock(t *testing.T) {
	ctx := context.Background()
	now := fixedInventoryNow()
	current := now
	store, err := storage.Open(ctx, storage.Options{
		Path: filepath.Join(t.TempDir(), "loopcoder.db"),
		Now:  func() time.Time { return current },
	})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	source := fixtureQuotaSource("codex", QuotaSourceFixture, false)
	remaining := int64(42)
	staleAfter := now.Add(10 * time.Minute)
	snapshot := normalizeQuotaSnapshot(QuotaSnapshot{
		QuotaSnapshotID:   quotaSnapshotID("codex", source.QuotaSourceID, "clock-crossing"),
		QuotaSourceID:     source.QuotaSourceID,
		SourceKind:        source.SourceKind,
		AdapterID:         "codex",
		ScopeKey:          "provider:codex/account:acct_fixture",
		QuantityKind:      QuantityRequests,
		Unit:              "request",
		WindowKind:        WindowRolling,
		RollingDurationMS: int64(time.Hour.Milliseconds()),
		ResetSemantics:    ResetRolling,
		RemainingValue:    &remaining,
		ValueScale:        0,
		Confidence:        ConfidenceExact,
		FieldConfidences:  map[string]Confidence{"remaining_value": ConfidenceExact},
		FreshnessState:    FreshnessFresh,
		CapturedAt:        formatTime(now.Add(-5 * time.Minute)),
		StaleAfter:        formatTime(staleAfter),
		ConflictSet:       []string{},
		GapReasons:        []string{},
		CreatedAt:         formatTime(now.Add(-5 * time.Minute)),
		UpdatedAt:         formatTime(now.Add(-5 * time.Minute)),
	})
	report := Report{
		SchemaVersion:         ProviderInventoryJSONSchema,
		GeneratedAt:           formatTime(now),
		Confidence:            ConfidenceExact,
		Installations:         []ProviderInstallation{},
		ProbeResults:          []ProbeResult{},
		AccountProfiles:       []AccountProfile{},
		AuthReadiness:         []AuthReadiness{},
		ModelCatalogSnapshots: []ModelCatalogSnapshot{},
		ModelCapabilities:     []ModelCapability{},
		QuotaTelemetrySources: []QuotaTelemetrySource{source},
		QuotaSnapshots:        []QuotaSnapshot{snapshot},
		GapReasons:            []string{},
	}
	if err := Refresh(ctx, store, report, now); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	loaded, err := Load(ctx, store)
	if err != nil {
		t.Fatalf("Load before stale_after: %v", err)
	}
	if got := loaded.QuotaSnapshots[0]; got.FreshnessState != FreshnessFresh || got.Confidence != ConfidenceExact {
		t.Fatalf("before stale_after = %#v, want fresh exact", got)
	}

	current = staleAfter.Add(time.Second)
	loaded, err = Load(ctx, store)
	if err != nil {
		t.Fatalf("Load after stale_after: %v", err)
	}
	if got := loaded.QuotaSnapshots[0]; got.FreshnessState != FreshnessStale || got.Confidence != ConfidenceStale || !containsString(got.GapReasons, "stale-cache") {
		t.Fatalf("after stale_after = %#v, want stale via injected clock", got)
	}
}

func TestQuotaSourceAllowlistRejectsForbiddenSources(t *testing.T) {
	source := fixtureQuotaSource("codex", QuotaSourceKind("private-web-ui-scrape"), false)
	err := ValidateQuotaTelemetrySource(source)
	if !errors.Is(err, ErrQuotaSourceForbidden) {
		t.Fatalf("ValidateQuotaTelemetrySource error = %v, want ErrQuotaSourceForbidden", err)
	}

	source = fixtureQuotaSource("codex", QuotaSourceOfficialCLICommand, false)
	source.Argv = []string{"codex", "quota", "$(hidden)"}
	err = ValidateQuotaTelemetrySource(source)
	if !errors.Is(err, ErrQuotaSourceForbidden) {
		t.Fatalf("shell-shaped argv error = %v, want ErrQuotaSourceForbidden", err)
	}

	source = fixtureQuotaSource("codex", QuotaSourceOfficialCLICommand, false)
	source.EnvironmentKeys = []string{"PATH", "FAKE_TOKEN_NAME"}
	err = ValidateQuotaTelemetrySource(source)
	if !errors.Is(err, ErrQuotaCredentialMaterial) {
		t.Fatalf("credential-shaped env name error = %v, want ErrQuotaCredentialMaterial", err)
	}
}

func TestQuotaNetworkDeclaredSourceRefusesWhenPermissionDenied(t *testing.T) {
	source := fixtureQuotaSource("codex", QuotaSourceOfficialCLICommand, true)
	now := fixedInventoryNow()
	snapshot := normalizeQuotaSnapshot(QuotaSnapshot{
		QuotaSnapshotID:   quotaSnapshotID("codex", source.QuotaSourceID, "network-denied"),
		QuotaSourceID:     source.QuotaSourceID,
		SourceKind:        source.SourceKind,
		AdapterID:         "codex",
		ScopeKey:          "provider:codex",
		QuantityKind:      QuantityRequests,
		Unit:              "request",
		WindowKind:        WindowUnknown,
		ResetSemantics:    ResetUnknown,
		ValueScale:        0,
		Confidence:        ConfidenceUnknown,
		FieldConfidences:  map[string]Confidence{"remaining_value": ConfidenceUnknown},
		FreshnessState:    FreshnessNotApplicable,
		CapturedAt:        formatTime(now),
		ConflictSet:       []string{},
		GapReasons:        []string{"network-denied"},
		TerminalErrorCode: "ErrTelemetryNetworkDenied",
	})
	err := ValidateQuotaSnapshot(source, snapshot)
	if !errors.Is(err, ErrTelemetryNetworkDenied) {
		t.Fatalf("ValidateQuotaSnapshot error = %v, want ErrTelemetryNetworkDenied", err)
	}
}

func TestQuotaConfidenceEnumsFailClosed(t *testing.T) {
	for _, value := range []Confidence{ConfidenceExact, ConfidenceEstimated, ConfidenceUnknown, ConfidenceUnavailable, ConfidenceStale} {
		var payload struct {
			Confidence Confidence `json:"confidence"`
		}
		data := []byte(`{"confidence":"` + string(value) + `"}`)
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatalf("confidence %q did not unmarshal: %v", value, err)
		}
	}
	var payload struct {
		Confidence Confidence `json:"confidence"`
	}
	err := json.Unmarshal([]byte(`{"confidence":"optimistic"}`), &payload)
	if !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("unknown confidence error = %v, want ErrInvalidRecord", err)
	}
}

func TestQuotaFreshnessTransitionMarksStale(t *testing.T) {
	source := fixtureQuotaSource("codex", QuotaSourceFixture, false)
	now := fixedInventoryNow()
	staleAt := now.Add(30 * time.Minute)
	snapshot := normalizeQuotaSnapshot(QuotaSnapshot{
		QuotaSnapshotID:  quotaSnapshotID("codex", source.QuotaSourceID, "stale"),
		QuotaSourceID:    source.QuotaSourceID,
		SourceKind:       source.SourceKind,
		AdapterID:        "codex",
		ScopeKey:         "provider:codex",
		QuantityKind:     QuantityRequests,
		Unit:             "request",
		WindowKind:       WindowProviderDefined,
		ResetSemantics:   ResetProviderDefined,
		ResetAt:          formatTime(staleAt),
		ValueScale:       0,
		Confidence:       ConfidenceExact,
		FieldConfidences: map[string]Confidence{"reset_at": ConfidenceExact},
		FreshnessState:   FreshnessFresh,
		CapturedAt:       formatTime(now),
		StaleAfter:       formatTime(staleAt),
	})
	got := markQuotaFreshness([]QuotaSnapshot{snapshot}, now.Add(time.Hour))[0]
	if got.FreshnessState != FreshnessStale || got.Confidence != ConfidenceStale || !containsString(got.GapReasons, "stale-cache") {
		t.Fatalf("stale transition = %#v", got)
	}
}

func TestQuotaSourceDisagreementLinksConflictSet(t *testing.T) {
	sourceA := fixtureQuotaSource("codex", QuotaSourceFixture, false)
	sourceA.SourceKey = "fixture-a"
	sourceA = normalizeQuotaTelemetrySource(sourceA)
	sourceB := fixtureQuotaSource("codex", QuotaSourceFixture, false)
	sourceB.SourceKey = "fixture-b"
	sourceB = normalizeQuotaTelemetrySource(sourceB)
	remainingA := int64(10)
	remainingB := int64(20)
	a := quotaConflictSnapshot(sourceA, "a", &remainingA)
	b := quotaConflictSnapshot(sourceB, "b", &remainingB)
	linked := LinkQuotaConflicts([]QuotaSnapshot{a, b})
	for _, snapshot := range linked {
		if len(snapshot.ConflictSet) != 1 || !containsString(snapshot.GapReasons, "provider-disagreement") {
			t.Fatalf("conflict not linked for %#v", snapshot)
		}
	}
}

func TestQuotaWindowShapesAndFailureHonestyAreRepresentable(t *testing.T) {
	source := fixtureQuotaSource("codex", QuotaSourceFixture, false)
	now := fixedInventoryNow()
	dstStart := now.AddDate(0, -4, -4).Add(5 * time.Hour)
	dstEnd := dstStart.Add(23 * time.Hour)
	cases := []QuotaSnapshot{
		{
			QuotaSnapshotID:  quotaSnapshotID("codex", source.QuotaSourceID, "fixed-day-dst"),
			QuotaSourceID:    source.QuotaSourceID,
			SourceKind:       source.SourceKind,
			AdapterID:        "codex",
			ScopeKey:         "provider:codex/account:acct_fixture",
			QuantityKind:     QuantityRequests,
			Unit:             "request",
			WindowKind:       WindowFixedDay,
			WindowStart:      formatTime(dstStart),
			WindowEnd:        formatTime(dstEnd),
			ResetAt:          formatTime(dstEnd),
			ResetSemantics:   ResetWindowBoundary,
			ValueScale:       0,
			Confidence:       ConfidenceEstimated,
			FieldConfidences: map[string]Confidence{"remaining_value": ConfidenceEstimated},
			FreshnessState:   FreshnessFresh,
			CapturedAt:       formatTime(dstStart.Add(2 * time.Hour)),
			GapReasons:       []string{"partial-scope"},
		},
		{
			QuotaSnapshotID:   quotaSnapshotID("codex", source.QuotaSourceID, "rolling"),
			QuotaSourceID:     source.QuotaSourceID,
			SourceKind:        source.SourceKind,
			AdapterID:         "codex",
			ScopeKey:          "provider:codex/model:gpt-fixture",
			QuantityKind:      QuantityTotalTokens,
			Unit:              "token",
			WindowKind:        WindowRolling,
			RollingDurationMS: int64((24 * time.Hour).Milliseconds()),
			ResetSemantics:    ResetRolling,
			ValueScale:        0,
			Confidence:        ConfidenceUnknown,
			FieldConfidences:  map[string]Confidence{"used_value": ConfidenceUnknown},
			FreshnessState:    FreshnessFresh,
			CapturedAt:        formatTime(now),
			GapReasons:        []string{"delayed"},
		},
		{
			QuotaSnapshotID:      quotaSnapshotID("codex", source.QuotaSourceID, "malformed"),
			QuotaSourceID:        source.QuotaSourceID,
			SourceKind:           source.SourceKind,
			AdapterID:            "codex",
			ScopeKey:             "provider:codex",
			QuantityKind:         QuantityProviderDefined,
			ProviderQuantityName: "requests_per_week",
			Unit:                 "requests_per_week",
			WindowKind:           WindowProviderDefined,
			ResetSemantics:       ResetUnknown,
			ValueScale:           0,
			Confidence:           ConfidenceUnavailable,
			FieldConfidences:     map[string]Confidence{"limit_value": ConfidenceUnavailable},
			FreshnessState:       FreshnessFresh,
			CapturedAt:           formatTime(now),
			GapReasons:           []string{"malformed-field"},
			TerminalErrorCode:    "ErrQuotaSnapshotMalformed",
		},
	}
	for _, snapshot := range cases {
		snapshot = normalizeQuotaSnapshot(snapshot)
		if err := ValidateQuotaSnapshot(source, snapshot); err != nil {
			t.Fatalf("ValidateQuotaSnapshot(%s): %v", snapshot.QuotaSnapshotID, err)
		}
	}
}

func TestDiscoverEmitsHonestUnsupportedQuotaSnapshotsForCurrentProviders(t *testing.T) {
	report, err := Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "codex", Verifier: "claude"}},
		Now:    fixedInventoryNow,
	}, fakeDeps(t, nil))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	byAdapter := map[string]QuotaSnapshot{}
	for _, snapshot := range report.QuotaSnapshots {
		byAdapter[snapshot.AdapterID] = snapshot
	}
	for _, adapterID := range []string{"codex", "claude"} {
		got, ok := byAdapter[adapterID]
		if !ok {
			t.Fatalf("missing quota snapshot for %s in %#v", adapterID, report.QuotaSnapshots)
		}
		wantGap := "unsupported-source"
		if adapterID == "codex" {
			wantGap = "quota-collection-not-granted"
		}
		if got.Confidence != ConfidenceUnavailable || got.FreshnessState != FreshnessNotApplicable || !containsString(got.GapReasons, wantGap) {
			t.Fatalf("%s quota snapshot = %#v, want honest unavailable unsupported-source", adapterID, got)
		}
	}
}

func fixtureQuotaSource(adapterID string, kind QuotaSourceKind, network bool) QuotaTelemetrySource {
	now := fixedInventoryNow()
	source := QuotaTelemetrySource{
		AdapterID:           adapterID,
		SourceKind:          kind,
		SourceKey:           "fixture-quota-v1",
		SourceSchemaVersion: "fixture.quota.v1",
		SupportedQuantities: []QuantityKind{QuantityRequests, QuantityTotalTokens, QuantityProviderDefined},
		SupportedWindows:    []WindowKind{WindowFixedHour, WindowFixedDay, WindowFixedWeek, WindowRolling, WindowProviderDefined, WindowUnknown},
		ScopeDimensions:     []string{"provider", "account", "model"},
		ConfidenceContract:  map[string]Confidence{"limit_value": ConfidenceExact, "used_value": ConfidenceEstimated, "remaining_value": ConfidenceUnknown, "reset_at": ConfidenceExact},
		NetworkDeclared:     network,
		EnvironmentKeys:     []string{"PATH"},
		TimeoutMS:           1000,
		OutputLimits:        defaultQuotaOutputLimits(),
		ClassificationRules: []string{"field-allowlist", "redacted-diagnostics-only"},
		CreatedAt:           formatTime(now),
		UpdatedAt:           formatTime(now),
		PolicyVersion:       PolicyVersion,
	}
	if network {
		source.NetworkPermissionScope = "provider:" + adapterID + "/action:quota-read/side-effect:read"
	}
	if kind == QuotaSourceOfficialCLICommand || kind == QuotaSourceOfficialCLIError {
		source.Argv = []string{adapterID, "quota", "--json"}
	}
	return normalizeQuotaTelemetrySource(source)
}

func quotaConflictSnapshot(source QuotaTelemetrySource, suffix string, remaining *int64) QuotaSnapshot {
	now := fixedInventoryNow()
	windowStart := now.AddDate(0, 0, -6)
	windowEnd := now.AddDate(0, 0, 1)
	return normalizeQuotaSnapshot(QuotaSnapshot{
		QuotaSnapshotID:  quotaSnapshotID("codex", source.QuotaSourceID, suffix),
		QuotaSourceID:    source.QuotaSourceID,
		SourceKind:       source.SourceKind,
		AdapterID:        "codex",
		ScopeKey:         "provider:codex/account:acct_fixture/model:gpt-fixture",
		QuantityKind:     QuantityRequests,
		Unit:             "request",
		WindowKind:       WindowFixedWeek,
		WindowStart:      formatTime(windowStart),
		WindowEnd:        formatTime(windowEnd),
		ResetAt:          formatTime(windowEnd),
		ResetSemantics:   ResetWindowBoundary,
		RemainingValue:   remaining,
		ValueScale:       0,
		Confidence:       ConfidenceExact,
		FieldConfidences: map[string]Confidence{"remaining_value": ConfidenceExact},
		FreshnessState:   FreshnessFresh,
		CapturedAt:       formatTime(now),
		ConflictSet:      []string{},
		GapReasons:       []string{},
	})
}
