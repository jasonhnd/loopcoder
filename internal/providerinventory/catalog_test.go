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

func TestStaticCatalogSnapshotPersistenceRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedInventoryNow})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	report, err := Discover(ctx, Options{Config: config.Config{}, Now: fixedInventoryNow}, fakeDeps(t, nil))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(report.ModelCatalogSnapshots) < 4 {
		t.Fatalf("model catalog snapshots = %d, want at least four provider static snapshots", len(report.ModelCatalogSnapshots))
	}
	seenSnapshot := map[string]bool{}
	for _, snapshot := range report.ModelCatalogSnapshots {
		seenSnapshot[snapshot.AdapterID] = true
		if snapshot.CatalogSourceKind != CatalogSourceAdapterDeclared || snapshot.InventoryFingerprint == "" {
			t.Fatalf("snapshot missing static provenance/fingerprint: %#v", snapshot)
		}
	}
	for _, provider := range []string{"antigravity", "claude", "codex", "gemini"} {
		if !seenSnapshot[provider] {
			t.Fatalf("missing static catalog snapshot for %s in %#v", provider, report.ModelCatalogSnapshots)
		}
	}
	if len(report.ModelCapabilities) == 0 {
		t.Fatal("model capabilities empty, want static registry records")
	}
	if err := Refresh(ctx, store, report, fixedInventoryNow()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	loaded, err := Load(ctx, store)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.ModelCatalogSnapshots) != len(report.ModelCatalogSnapshots) || len(loaded.ModelCapabilities) != len(report.ModelCapabilities) {
		t.Fatalf("loaded catalog counts = %d/%d, want %d/%d", len(loaded.ModelCatalogSnapshots), len(loaded.ModelCapabilities), len(report.ModelCatalogSnapshots), len(report.ModelCapabilities))
	}
	for _, capability := range loaded.ModelCapabilities {
		if capability.ModelCatalogSnapshotID == "" || capability.ModelCapabilityID == "" || len(capability.EntrySources) == 0 {
			t.Fatalf("loaded capability missing durable identity/provenance: %#v", capability)
		}
	}
}

func TestCatalogLifecycleTransitionsAliasesAndConflictsPreserveSources(t *testing.T) {
	now := fixedInventoryNow()
	adapter := AdapterDeclaration{AdapterID: "fixture", DisplayName: "fixture", ExecutableNames: []string{"fixture"}}
	snapshot, capabilities, err := buildCatalogSnapshot(adapter, nil, []CatalogSourceInput{
		{
			Kind:           CatalogSourceAdapterDeclared,
			Reference:      "static-fixture",
			Precedence:     10,
			Confidence:     ConfidenceExact,
			FreshnessState: FreshnessFresh,
			Entries: []CatalogInputEntry{
				{CanonicalModelID: "renamed-model", Aliases: []string{"old-model"}, LifecycleState: LifecycleRenamed, ReplacementModelID: "new-model", AvailabilityState: AvailabilityAvailable, ReadOnly: CapabilityTrue},
				{CanonicalModelID: "deprecated-model", LifecycleState: LifecycleDeprecated, ReplacementModelID: "replacement-model", AvailabilityState: AvailabilityAvailable},
				{CanonicalModelID: "removed-model", LifecycleState: LifecycleRemoved, AvailabilityState: AvailabilityRemoved},
				{CanonicalModelID: "restricted-model", LifecycleState: LifecycleAvailable, AvailabilityState: AvailabilityAccountRestricted},
				{CanonicalModelID: "temporary-model", LifecycleState: LifecycleAvailable, AvailabilityState: AvailabilityTemporarilyUnavailable},
			},
		},
		{
			Kind:           CatalogSourceConfiguredOverlay,
			Reference:      "operator-overlay",
			Precedence:     100,
			Confidence:     ConfidenceExact,
			FreshnessState: FreshnessFresh,
			Entries: []CatalogInputEntry{
				{CanonicalModelID: "renamed-model", Aliases: []string{"legacy-model"}, LifecycleState: LifecycleAvailable, AvailabilityState: AvailabilityAvailable, ReadOnly: CapabilityFalse},
			},
		},
	}, now)
	if err != nil {
		t.Fatalf("buildCatalogSnapshot: %v", err)
	}
	if snapshot.ConflictCount == 0 {
		t.Fatalf("conflict count = 0, want preserved conflicts")
	}
	byModel := map[string]ModelCapability{}
	for _, capability := range capabilities {
		byModel[capability.CanonicalModelID] = capability
	}
	renamed := byModel["renamed-model"]
	if renamed.LifecycleState != LifecycleAvailable || len(renamed.Conflicts) == 0 || len(renamed.EntrySources) != 2 {
		t.Fatalf("renamed conflict merge = %#v", renamed)
	}
	if len(renamed.Aliases) != 1 || renamed.Aliases[0].Alias != "legacy-model" {
		t.Fatalf("aliases = %#v, want chosen source aliases retained with provenance", renamed.Aliases)
	}
	if byModel["deprecated-model"].LifecycleState != LifecycleDeprecated || byModel["removed-model"].LifecycleState != LifecycleRemoved {
		t.Fatalf("lifecycle entries = %#v", byModel)
	}
	if byModel["restricted-model"].AvailabilityState != AvailabilityAccountRestricted || byModel["temporary-model"].AvailabilityState != AvailabilityTemporarilyUnavailable {
		t.Fatalf("availability entries = %#v", byModel)
	}
}

func TestCatalogUnknownAndStaleCapabilitiesFailClosed(t *testing.T) {
	fresh := ModelCapability{
		LifecycleState:      LifecycleAvailable,
		AvailabilityState:   AvailabilityAvailable,
		FreshnessState:      FreshnessFresh,
		Confidence:          ConfidenceExact,
		ReadOnly:            CapabilityUnknown,
		JSONOutput:          CapabilityTrue,
		Cancellation:        CapabilityTrue,
		NestedSubagents:     CapabilityTrue,
		MCPConfig:           CapabilityTrue,
		TokenUsageReporting: CapabilityTrue,
		ImageInput:          CapabilityTrue,
		ImageOutput:         CapabilityTrue,
	}
	if fresh.SatisfiesHardRequirements(HardRequirement{ReadOnly: true}) {
		t.Fatal("unknown read_only satisfied hard requirement")
	}
	fresh.ReadOnly = CapabilityTrue
	if !fresh.SatisfiesHardRequirements(HardRequirement{ReadOnly: true, JSONOutput: true, Cancellation: true}) {
		t.Fatal("fresh exact true capabilities did not satisfy hard requirements")
	}
	fresh.FreshnessState = FreshnessStale
	fresh.Confidence = ConfidenceStale
	if fresh.SatisfiesHardRequirements(HardRequirement{ReadOnly: true}) {
		t.Fatal("stale capability satisfied hard requirement")
	}
}

func TestNetworkDeclaredCatalogListingSkippedByDefault(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, executableName("agy"))
	writeExecutable(t, exe)
	deps := fakeDeps(t, map[string]string{filepath.Clean(exe): "agy 1.0.0"})
	deps.Getenv = func(key string) string {
		if key == "PATH" {
			return dir
		}
		return ""
	}
	calls := 0
	deps.RunProbe = func(_ context.Context, req ProbeExecution) (ProbeExecutionResult, error) {
		calls++
		if len(req.Argv) >= 2 && req.Argv[0] == "agy" && req.Argv[1] == "models" {
			t.Fatal("network-declared catalog command was executed")
		}
		return ProbeExecutionResult{Stdout: "agy 1.0.0\n", ExitCode: 0}, nil
	}
	report, err := Discover(context.Background(), Options{Config: config.Config{Adapters: config.Adapters{Worker: "antigravity"}}, Now: fixedInventoryNow}, deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if calls == 0 {
		t.Fatal("expected install/auth probes to run for installed antigravity fixture")
	}
	found := false
	for _, probe := range report.ProbeResults {
		if probe.AdapterID == "antigravity" && probe.ProbeKind == "catalog" {
			found = true
			if !probe.NetworkDeclared || probe.NetworkPermission != NetworkDenied || !contains(probe.GapReasons, "network-permission-denied") {
				t.Fatalf("catalog skip probe = %#v", probe)
			}
		}
	}
	if !found {
		t.Fatalf("missing skipped catalog probe in %#v", report.ProbeResults)
	}
	if !contains(report.GapReasons, "provider-antigravity-catalog-network-permission-denied") {
		t.Fatalf("report gaps = %#v", report.GapReasons)
	}
	seenCodexCapability := false
	for _, capability := range report.ModelCapabilities {
		if capability.AdapterID == "codex" {
			seenCodexCapability = true
		}
	}
	if !seenCodexCapability {
		t.Fatalf("network-declared catalog skip removed unrelated static catalog capabilities: %#v", report.ModelCapabilities)
	}
}

func TestCatalogEnumsFailClosedOnUnknownValue(t *testing.T) {
	var payload ModelCapability
	err := json.Unmarshal([]byte(`{"lifecycle_state":"beta","availability_state":"available","read_only":"true"}`), &payload)
	if !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("json.Unmarshal lifecycle error = %v, want ErrInvalidRecord", err)
	}
	err = json.Unmarshal([]byte(`{"lifecycle_state":"available","availability_state":"mystery","read_only":"true"}`), &payload)
	if !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("json.Unmarshal availability error = %v, want ErrInvalidRecord", err)
	}
	err = json.Unmarshal([]byte(`{"lifecycle_state":"available","availability_state":"available","read_only":"maybe"}`), &payload)
	if !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("json.Unmarshal capability error = %v, want ErrInvalidRecord", err)
	}
}

func TestCatalogSnapshotMarksExpiredEntriesStale(t *testing.T) {
	now := fixedInventoryNow()
	adapter := AdapterDeclaration{AdapterID: "fixture", DisplayName: "fixture", ExecutableNames: []string{"fixture"}}
	snapshot, capabilities, err := buildCatalogSnapshot(adapter, nil, []CatalogSourceInput{{
		Kind:           CatalogSourceFixture,
		Reference:      "stale-fixture",
		Precedence:     1,
		Confidence:     ConfidenceExact,
		FreshnessState: FreshnessFresh,
		Entries:        []CatalogInputEntry{{CanonicalModelID: "model", AvailabilityState: AvailabilityAvailable, ReadOnly: CapabilityTrue}},
	}}, now)
	if err != nil {
		t.Fatalf("buildCatalogSnapshot: %v", err)
	}
	snapshot.StaleAfter = formatTime(now.Add(-time.Second))
	snapshot, capabilities = markCatalogFreshness(snapshot, capabilities, now)
	if snapshot.FreshnessState != FreshnessStale || snapshot.Confidence != ConfidenceStale {
		t.Fatalf("snapshot freshness = %#v", snapshot)
	}
	if len(capabilities) != 1 || capabilities[0].FreshnessState != FreshnessStale || capabilities[0].Confidence != ConfidenceStale {
		t.Fatalf("capabilities freshness = %#v", capabilities)
	}
}
