package providerinventory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/runtimecap"
)

func TestAdapterConformanceFutureProviderFixturePipeline(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	exe := filepath.Join(binDir, executableName("futurefixture"))
	writeExecutable(t, exe)

	contract := runtimecap.DefaultContract()
	contract.Providers = append(contract.Providers, runtimecap.ProviderRuntime{
		Name:                "futurefixture",
		AdapterVersion:      "v1",
		DisplayName:         "Future Fixture",
		Vendor:              "LoopCoder Fixture",
		Executable:          "futurefixture",
		VersionArgv:         []string{"--version"},
		ReadOnly:            true,
		JSONOutput:          true,
		MCPConfig:           true,
		Cancellation:        true,
		TokenUsageReporting: true,
		AuthProbeCommand:    []string{"futurefixture", "auth", "status"},
		AuthProbeParser:     "fixture-text-status",
		StaticModelCatalog: []runtimecap.ProviderModelCapability{{
			ModelID:             "future-model",
			DisplayName:         "Future Model",
			ReadOnly:            true,
			JSONOutput:          true,
			MCPConfig:           true,
			Cancellation:        true,
			TokenUsageReporting: true,
			RolesSupported:      []runtimecap.CompatibilityRole{runtimecap.RoleWorker, runtimecap.RoleVerifier},
			AvailabilityState:   string(AvailabilityAvailable),
			LifecycleState:      string(LifecycleAvailable),
		}},
	})

	deps := fakeDeps(t, nil)
	deps.Getenv = func(key string) string {
		switch key {
		case "PATH":
			return binDir
		case "PATHEXT":
			return ".EXE;.CMD;.BAT"
		case "LOOPCODER_TEST_TOKEN":
			return "runtime-" + "opaque-value"
		default:
			return ""
		}
	}
	var sawVersionProbe, sawAuthProbe bool
	deps.RunProbe = func(_ context.Context, req ProbeExecution) (ProbeExecutionResult, error) {
		for _, env := range req.Env {
			if strings.Contains(env, "LOOPCODER_TEST_TOKEN") || strings.Contains(env, "opaque-value") {
				t.Fatalf("credential-like env reached adapter probe: %q", env)
			}
		}
		if strings.Contains(strings.Join(req.Argv, " "), root+string(filepath.Separator)+"unrelated") {
			t.Fatalf("adapter probe received unrelated repo path in typed input: %#v", req.Argv)
		}
		switch {
		case len(req.Argv) == 2 && req.Argv[1] == "--version":
			sawVersionProbe = true
			return ProbeExecutionResult{Stdout: "futurefixture 4.0.0\n", ExitCode: 0}, nil
		case len(req.Argv) == 3 && req.Argv[1] == "auth" && req.Argv[2] == "status":
			sawAuthProbe = true
			return ProbeExecutionResult{Stdout: "profile future ready\n", ExitCode: 0}, nil
		default:
			t.Fatalf("unexpected probe argv: %#v", req.Argv)
			return ProbeExecutionResult{ExitCode: 2}, nil
		}
	}

	report, err := Discover(context.Background(), Options{
		RepoPath:        filepath.Join(root, "unrelated"),
		Config:          config.Config{Adapters: config.Adapters{Worker: "futurefixture"}},
		RuntimeContract: contract,
		Now:             fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if !sawVersionProbe || !sawAuthProbe {
		t.Fatalf("probe coverage version=%v auth=%v, want both", sawVersionProbe, sawAuthProbe)
	}
	installation := installationForAdapter(t, report, "futurefixture")
	if installation.Version != "futurefixture 4.0.0" || installation.AdapterDeclarationID == "" {
		t.Fatalf("future fixture installation = %#v", installation)
	}
	readiness := latestAuthReadinessFor(t, report, "futurefixture")
	if readiness.ReadinessState != ReadinessReady || readiness.EvidenceKind != EvidenceStatusCommand {
		t.Fatalf("future fixture readiness = %#v, want ready status command", readiness)
	}
	capability := capabilityForAdapterModel(t, report, "futurefixture", "future-model")
	if !capability.SatisfiesHardRequirements(HardRequirement{ReadOnly: true, JSONOutput: true, Cancellation: true}) {
		t.Fatalf("future fixture capability did not satisfy declared hard requirements: %#v", capability)
	}
}

func TestAdapterConformanceRejectsMalformedDeclarations(t *testing.T) {
	tests := []struct {
		name string
		decl AdapterDeclaration
		want string
	}{
		{
			name: "empty executable",
			decl: AdapterDeclaration{AdapterID: "fixture", ExecutableNames: []string{""}, AuthUnsupportedReason: "unsupported"},
			want: "executable_names[0]",
		},
		{
			name: "path separator in command",
			decl: AdapterDeclaration{AdapterID: "fixture", ExecutableNames: []string{"fixture"}, AuthProbeCommand: []string{"bin/fixture", "auth"}},
			want: "auth_probe_command[0]",
		},
		{
			name: "network true without command",
			decl: AdapterDeclaration{AdapterID: "fixture", ExecutableNames: []string{"fixture"}, MayNetwork: true, AuthUnsupportedReason: "unsupported"},
			want: "may_network",
		},
		{
			name: "unsupported schema",
			decl: AdapterDeclaration{AdapterID: "fixture", DeclarationSchemaVersion: "loopcoder.adapter_declaration.v99", ExecutableNames: []string{"fixture"}, AuthUnsupportedReason: "unsupported"},
			want: "unsupported",
		},
		{
			name: "catalog missing provenance",
			decl: AdapterDeclaration{AdapterID: "fixture", ExecutableNames: []string{"fixture"}, CatalogProbeMayNetwork: true, AuthUnsupportedReason: "unsupported"},
			want: "catalog_probe_may_network",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			violations := ValidateAdapterDeclaration(tt.decl)
			if len(violations) == 0 {
				t.Fatalf("ValidateAdapterDeclaration returned no violations for %#v", tt.decl)
			}
			if !strings.Contains(strings.Join(violations, "; "), tt.want) {
				t.Fatalf("violations = %#v, want substring %q", violations, tt.want)
			}
		})
	}
}

func TestAdapterConformanceCoversPartialMalformedTimeoutCancellationAndRedaction(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, executableName("fixture"))
	writeExecutable(t, exe)
	adapter := AdapterDeclaration{
		AdapterID:                "fixture",
		AdapterVersion:           "v1",
		DeclarationSchemaVersion: AdapterDeclarationSchema,
		ConformanceVersion:       "loopcoder.adapter_conformance.v1",
		DisplayName:              "Fixture",
		Vendor:                   "LoopCoder Fixture",
		ExecutableNames:          []string{"fixture"},
		VersionArgv:              []string{"--version"},
		AuthProbeCommand:         []string{"fixture", "auth", "status"},
		AuthProbeParser:          "fixture-text-status",
	}
	if violations := ValidateAdapterDeclaration(adapter); len(violations) > 0 {
		t.Fatalf("valid fixture adapter violations: %#v", violations)
	}

	deps := fakeDeps(t, nil)
	deps.RunProbe = func(context.Context, ProbeExecution) (ProbeExecutionResult, error) {
		return ProbeExecutionResult{Stdout: "{malformed", ExitCode: 0}, nil
	}
	_, readiness, probe := inspectAuthReadiness(context.Background(), nil, adapter, candidate{path: exe, source: DiscoveryFixture}, "pinst_fixture", fixedInventoryNow(), deps)
	if probe == nil || !contains(probe.GapReasons, "auth-status-unrecognized") || len(readiness) != 1 || readiness[0].ReadinessState != ReadinessUnknown || readiness[0].TerminalErrorCode != "ErrAuthStatusUnrecognized" {
		t.Fatalf("malformed auth output probe=%#v readiness=%#v, want schema mismatch unknown", probe, readiness)
	}

	deps.RunProbe = func(context.Context, ProbeExecution) (ProbeExecutionResult, error) {
		return ProbeExecutionResult{ExitCode: -1, TimedOut: true, Killed: true}, nil
	}
	_, readiness, probe = inspectAuthReadiness(context.Background(), nil, adapter, candidate{path: exe, source: DiscoveryFixture}, "pinst_fixture", fixedInventoryNow(), deps)
	if probe == nil || !probe.TimedOut || !probe.Killed || probe.TerminalErrorCode != "ErrAuthProbeTimeout" || readiness[0].TerminalErrorCode != "ErrAuthProbeTimeout" {
		t.Fatalf("timeout/cancellation probe=%#v readiness=%#v", probe, readiness)
	}

	partial := AdapterDeclaration{
		AdapterID:                "partial",
		AdapterVersion:           "v1",
		DeclarationSchemaVersion: AdapterDeclarationSchema,
		DisplayName:              "Partial",
		Vendor:                   "LoopCoder Fixture",
		ExecutableNames:          []string{"partial"},
		AuthUnsupportedReason:    "partial adapter has no safe auth readiness command",
	}
	if violations := ValidateAdapterDeclaration(partial); len(violations) > 0 {
		t.Fatalf("partial adapter should be explicitly supported as unknown: %#v", violations)
	}
	unsupported := unsupportedAuthReadiness(partial, nil, fixedInventoryNow(), partial.AuthUnsupportedReason)
	if unsupported.ReadinessState != ReadinessUnknown || unsupported.EvidenceKind != EvidenceUnsupported || unsupported.UnsupportedReason == "" {
		t.Fatalf("unsupported readiness = %#v, want explicit unknown unsupported", unsupported)
	}

	secretish := "api_" + "key=" + strings.Repeat("x", 20)
	redacted, findings := redactProviderOutput("provider said " + secretish)
	if findings == 0 || strings.Contains(redacted, secretish) {
		t.Fatalf("redaction failed findings=%d output=%q", findings, redacted)
	}
}

func TestAdapterDeclarationVersionUpgradeChangesIdentity(t *testing.T) {
	base := normalizeAdapterDeclaration(AdapterDeclaration{
		AdapterID:             "fixture",
		DisplayName:           "Fixture",
		Vendor:                "LoopCoder Fixture",
		ExecutableNames:       []string{"fixture"},
		AuthUnsupportedReason: "unsupported",
		ConformanceVersion:    "loopcoder.adapter_conformance.v1",
	})
	upgraded := base
	upgraded.AdapterVersion = "v2"
	if adapterDeclarationID(base) == adapterDeclarationID(upgraded) {
		t.Fatalf("adapter declaration id did not change across version upgrade: %s", adapterDeclarationID(base))
	}
	upgraded.DeclarationSchemaVersion = "loopcoder.adapter_declaration.v99"
	if violations := ValidateAdapterDeclaration(upgraded); len(violations) == 0 {
		t.Fatal("unknown declaration schema version passed validation")
	}
}

func installationForAdapter(t *testing.T, report Report, adapterID string) ProviderInstallation {
	t.Helper()
	for _, installation := range report.Installations {
		if installation.AdapterID == adapterID {
			return installation
		}
	}
	t.Fatalf("installation for %s missing in %#v", adapterID, report.Installations)
	return ProviderInstallation{}
}

func capabilityForAdapterModel(t *testing.T, report Report, adapterID, modelID string) ModelCapability {
	t.Helper()
	for _, capability := range report.ModelCapabilities {
		if capability.AdapterID == adapterID && capability.CanonicalModelID == modelID {
			return capability
		}
	}
	t.Fatalf("model capability %s/%s missing in %#v", adapterID, modelID, report.ModelCapabilities)
	return ModelCapability{}
}
