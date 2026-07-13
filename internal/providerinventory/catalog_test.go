package providerinventory

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
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
	if len(report.ModelCatalogSnapshots) < 5 {
		t.Fatalf("model catalog snapshots = %d, want at least five provider static snapshots", len(report.ModelCatalogSnapshots))
	}
	seenSnapshot := map[string]bool{}
	for _, snapshot := range report.ModelCatalogSnapshots {
		if snapshot.CatalogSourceKind != CatalogSourceAdapterDeclared {
			continue
		}
		seenSnapshot[snapshot.AdapterID] = true
		if snapshot.InventoryFingerprint == "" {
			t.Fatalf("snapshot missing static provenance/fingerprint: %#v", snapshot)
		}
	}
	for _, provider := range []string{"antigravity", "claude", "codex", "gemini", "grok"} {
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
	if len(loaded.ModelCatalogSnapshots) < len(seenSnapshot) || len(loaded.ModelCapabilities) != len(report.ModelCapabilities) {
		t.Fatalf("loaded catalog counts = %d/%d, want at least %d snapshots and %d capabilities", len(loaded.ModelCatalogSnapshots), len(loaded.ModelCapabilities), len(seenSnapshot), len(report.ModelCapabilities))
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

func TestDiscoverGrokBuildDynamicCatalogAuthAndSecretBoundaries(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, executableName("grok"))
	writeExecutable(t, exe)
	accessKeyCanary := "AKIA" + strings.Repeat("A", 16)
	tokenCanary := "xai_token=" + strings.Repeat("b", 24)
	unrelatedCanary := "unrelated-" + strings.Repeat("c", 16)

	deps := fakeDeps(t, nil)
	deps.Getenv = func(key string) string {
		switch key {
		case "PATH":
			return dir
		case "XAI_API_KEY":
			return accessKeyCanary
		case "UNRELATED_SECRET_VALUE":
			return unrelatedCanary
		default:
			return ""
		}
	}
	var sawVersion, sawModels bool
	deps.RunProbe = func(_ context.Context, req ProbeExecution) (ProbeExecutionResult, error) {
		for _, env := range req.Env {
			if strings.Contains(env, accessKeyCanary) || strings.Contains(env, unrelatedCanary) || strings.Contains(env, "XAI_API_KEY") || strings.Contains(env, "UNRELATED_SECRET_VALUE") {
				t.Fatalf("secret or unrelated env reached grok probe: %q", env)
			}
		}
		joined := strings.Join(req.Argv, " ")
		for _, forbidden := range []string{"login", "update", "inspect", "plugin", "-p", "--prompt"} {
			if strings.Contains(joined, forbidden) {
				t.Fatalf("grok probe used forbidden argv %q in %#v", forbidden, req.Argv)
			}
		}
		switch {
		case len(req.Argv) == 2 && req.Argv[1] == "version":
			sawVersion = true
			return ProbeExecutionResult{Stdout: "grok 0.1.211\n", ExitCode: 0}, nil
		case len(req.Argv) == 2 && req.Argv[1] == "models":
			sawModels = true
			return ProbeExecutionResult{
				Stdout:   `{"models":[{"id":"grok-4.5","name":"Grok 4.5","aliases":["grok-build","default"]},{"id":"openai-main/gpt-5.4","name":"GPT 5.4","alias":"tfy-gpt","provider":"openai","base_url":"https://gateway.example.test/v1","custom":true}]}`,
				Stderr:   tokenCanary,
				ExitCode: 0,
			}, nil
		default:
			t.Fatalf("unexpected grok probe argv: %#v", req.Argv)
			return ProbeExecutionResult{ExitCode: 2}, nil
		}
	}

	report, err := Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "grok"}},
		Now:    fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !sawVersion || !sawModels {
		t.Fatalf("probe coverage version=%v models=%v", sawVersion, sawModels)
	}
	installation := installationForAdapter(t, report, "grok")
	if installation.InstallationState != InstallationInstalled || installation.Version != "grok 0.1.211" {
		t.Fatalf("grok installation = %#v", installation)
	}
	readiness := latestAuthReadinessFor(t, report, "grok")
	if readiness.ReadinessState != ReadinessReady || readiness.EvidenceKind != EvidenceStatusCommand || !contains(readiness.GapReasons, "authorization-scope-unknown") {
		t.Fatalf("grok readiness = %#v", readiness)
	}
	xai := capabilityForAdapterModel(t, report, "grok", "grok-4.5")
	if len(xai.Aliases) != 2 || xai.EntrySources[0].SourceReference != "grok-models:xai" {
		t.Fatalf("xai capability aliases/source = %#v", xai)
	}
	custom := capabilityForAdapterModel(t, report, "grok", "openai-main/gpt-5.4")
	if len(custom.EntrySources) == 0 || custom.EntrySources[0].SourceReference != "grok-models:custom:openai" || !contains(custom.Constraints, "provider_attribution=custom-non-xai:openai") {
		t.Fatalf("custom capability attribution = %#v", custom)
	}
	catalogProbe := findProbe(t, report, "grok", "catalog")
	if catalogProbe.Outcome != OutcomeInstalled || catalogProbe.ParsedFields["model_count"] != "2" || catalogProbe.SecretFindingCount == 0 {
		t.Fatalf("catalog probe = %#v", catalogProbe)
	}
	payload, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("json.Marshal report: %v", err)
	}
	for _, forbidden := range []string{accessKeyCanary, tokenCanary, unrelatedCanary} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("provider inventory persisted secret canary %q in %s", forbidden, payload)
		}
	}
}

func TestDiscoverGrokBuildNotInstalledDoesNotRunProbe(t *testing.T) {
	deps := fakeDeps(t, nil)
	deps.Getenv = func(string) string { return "" }
	deps.RunProbe = func(context.Context, ProbeExecution) (ProbeExecutionResult, error) {
		t.Fatal("RunProbe called for absent grok executable")
		return ProbeExecutionResult{}, nil
	}
	report, err := Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "grok"}},
		Now:    fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, installation := range report.Installations {
		if installation.AdapterID == "grok" {
			t.Fatalf("grok installation discovered on empty PATH: %#v", installation)
		}
	}
	probe := findProbe(t, report, "grok", "install")
	if probe.Outcome != OutcomeNotInstalled || probe.ProbeMethod != ProbeMethodLookPath || !contains(probe.GapReasons, "executable-not-found") {
		t.Fatalf("absent grok probe = %#v", probe)
	}
}

func TestDiscoverGrokBuildFailureBranches(t *testing.T) {
	tests := []struct {
		name            string
		versionStdout   string
		modelsStdout    string
		modelsExitCode  int
		modelsTimedOut  bool
		wantInstall     InstallationState
		wantInstallCode string
		wantCatalogCode string
		wantCatalogGap  string
		wantModelsCall  bool
	}{
		{
			name:            "unsupported old version",
			versionStdout:   "grok 0.0.9\n",
			wantInstall:     InstallationInstalledUnusable,
			wantInstallCode: "ErrUnsupportedVersion",
			wantCatalogCode: "ErrUnsupportedVersion",
			wantCatalogGap:  "installation-not-usable",
		},
		{
			name:            "malformed version output",
			versionStdout:   "definitely not a grok build version\n",
			wantInstall:     InstallationInstalledUnusable,
			wantInstallCode: "ErrProbeUnparseableVersion",
			wantCatalogCode: "ErrProbeUnparseableVersion",
			wantCatalogGap:  "installation-not-usable",
		},
		{
			name:            "malformed catalog",
			versionStdout:   "grok 0.1.211\n",
			modelsStdout:    "{malformed",
			wantInstall:     InstallationInstalled,
			wantCatalogCode: "ErrCatalogMalformedOutput",
			wantCatalogGap:  "catalog-output-malformed",
			wantModelsCall:  true,
		},
		{
			name:            "network unavailable",
			versionStdout:   "grok 0.1.211\n",
			modelsStdout:    "network timeout connecting to cli-chat-proxy.grok.com",
			modelsExitCode:  1,
			wantInstall:     InstallationInstalled,
			wantCatalogCode: "ErrCatalogNetworkUnavailable",
			wantCatalogGap:  "catalog-network-unavailable",
			wantModelsCall:  true,
		},
		{
			name:            "timeout",
			versionStdout:   "grok 0.1.211\n",
			modelsTimedOut:  true,
			wantInstall:     InstallationInstalled,
			wantCatalogCode: "ErrCatalogProbeTimeout",
			wantCatalogGap:  "catalog-probe-timeout",
			wantModelsCall:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			exe := filepath.Join(dir, executableName("grok"))
			writeExecutable(t, exe)
			deps := fakeDeps(t, nil)
			deps.Getenv = func(key string) string {
				if key == "PATH" {
					return dir
				}
				return ""
			}
			modelsCalls := 0
			deps.RunProbe = func(_ context.Context, req ProbeExecution) (ProbeExecutionResult, error) {
				switch {
				case len(req.Argv) == 2 && req.Argv[1] == "version":
					return ProbeExecutionResult{Stdout: tt.versionStdout, ExitCode: 0}, nil
				case len(req.Argv) == 2 && req.Argv[1] == "models":
					modelsCalls++
					if tt.modelsTimedOut {
						return ProbeExecutionResult{TimedOut: true, Killed: true, ExitCode: -1}, nil
					}
					return ProbeExecutionResult{Stdout: tt.modelsStdout, ExitCode: tt.modelsExitCode}, nil
				default:
					t.Fatalf("unexpected probe argv: %#v", req.Argv)
					return ProbeExecutionResult{ExitCode: 2}, nil
				}
			}
			report, err := Discover(context.Background(), Options{
				Config: config.Config{Adapters: config.Adapters{Worker: "grok"}},
				Now:    fixedInventoryNow,
			}, deps)
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			installation := installationForAdapter(t, report, "grok")
			if installation.InstallationState != tt.wantInstall || installation.TerminalErrorCode != tt.wantInstallCode {
				t.Fatalf("installation = %#v", installation)
			}
			if tt.wantModelsCall && modelsCalls == 0 {
				t.Fatal("grok models was not called")
			}
			if !tt.wantModelsCall && modelsCalls != 0 {
				t.Fatalf("grok models calls = %d, want zero", modelsCalls)
			}
			catalogProbe := findProbe(t, report, "grok", "catalog")
			if catalogProbe.TerminalErrorCode != tt.wantCatalogCode || !contains(catalogProbe.GapReasons, tt.wantCatalogGap) {
				t.Fatalf("catalog probe = %#v, want code=%s gap=%s", catalogProbe, tt.wantCatalogCode, tt.wantCatalogGap)
			}
		})
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
