package runtimecap_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/runtimecap"
)

func TestDefaultContractInvariants(t *testing.T) {
	if violations := runtimecap.DefaultContract().InvariantViolations(); len(violations) > 0 {
		t.Fatalf("runtime capability contract invariants failed: %v", violations)
	}
}

func TestDefaultContractRepresentsExistingProviders(t *testing.T) {
	if got, want := runtimecap.ProviderNames(), []string{"antigravity", "claude", "codex", "gemini", "grok"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ProviderNames = %#v, want %#v", got, want)
	}

	tests := []struct {
		provider   string
		executable string
		readOnly   bool
		bounded    bool
		mcp        bool
		json       bool
		usage      bool
	}{
		{provider: "codex", executable: "codex", readOnly: true, bounded: true, mcp: true, json: true, usage: true},
		{provider: "claude", executable: "claude", readOnly: true, mcp: true, json: true, usage: true},
		{provider: "gemini", executable: "gemini", readOnly: true, mcp: true, json: true, usage: true},
		{provider: "antigravity", executable: "agy", readOnly: false, mcp: false, json: false, usage: false},
		{provider: "grok", executable: "grok", readOnly: true, bounded: true, mcp: false, json: true, usage: true},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			provider, ok := runtimecap.LookupProvider(tt.provider)
			if !ok {
				t.Fatalf("LookupProvider(%q) returned false", tt.provider)
			}
			if provider.Executable != tt.executable ||
				provider.Supports(runtimecap.ProviderReadOnly) != tt.readOnly ||
				provider.Supports(runtimecap.ProviderBoundedWrite) != tt.bounded ||
				provider.Supports(runtimecap.ProviderMCPConfig) != tt.mcp ||
				provider.Supports(runtimecap.ProviderJSONOutput) != tt.json ||
				provider.Supports(runtimecap.ProviderTokenUsage) != tt.usage {
				t.Fatalf("provider capability = %#v", provider)
			}
		})
	}
}

func TestDefaultContractDeclaresCredentialBlindAuthReadiness(t *testing.T) {
	tests := []struct {
		provider string
		command  []string
		parser   string
		network  bool
		envNames []string
		paths    []string
	}{
		{
			provider: "codex",
			command:  []string{"codex", "login", "status"},
			parser:   "codex-login-status",
		},
		{
			provider: "claude",
			command:  []string{"claude", "auth", "status", "--json"},
			parser:   "claude-auth-status-json",
		},
		{
			provider: "gemini",
			envNames: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"},
			paths:    []string{"~/.gemini/oauth_creds.json"},
		},
		{
			provider: "antigravity",
			command:  []string{"agy", "models"},
			parser:   "agy-models",
			network:  true,
		},
		{
			provider: "grok",
			command:  []string{"grok", "models"},
			parser:   "grok-models",
			network:  true,
			envNames: []string{"XAI_API_KEY"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			provider, ok := runtimecap.LookupProvider(tt.provider)
			if !ok {
				t.Fatalf("LookupProvider(%q) returned false", tt.provider)
			}
			if !reflect.DeepEqual(provider.AuthProbeCommand, tt.command) {
				t.Fatalf("AuthProbeCommand = %#v, want %#v", provider.AuthProbeCommand, tt.command)
			}
			if provider.AuthProbeParser != tt.parser {
				t.Fatalf("AuthProbeParser = %q, want %q", provider.AuthProbeParser, tt.parser)
			}
			if provider.MayNetwork != tt.network {
				t.Fatalf("MayNetwork = %v, want %v", provider.MayNetwork, tt.network)
			}
			if !reflect.DeepEqual(provider.AuthEnvironmentNames, tt.envNames) {
				t.Fatalf("AuthEnvironmentNames = %#v, want %#v", provider.AuthEnvironmentNames, tt.envNames)
			}
			if !reflect.DeepEqual(provider.AuthArtifactPaths, tt.paths) {
				t.Fatalf("AuthArtifactPaths = %#v, want %#v", provider.AuthArtifactPaths, tt.paths)
			}
		})
	}
}

func TestDefaultContractDeclaresCatalogProbeProvenance(t *testing.T) {
	antigravity, ok := runtimecap.LookupProvider("antigravity")
	if !ok {
		t.Fatal("LookupProvider(\"antigravity\") returned false")
	}
	if !reflect.DeepEqual(antigravity.CatalogProbeCommand, []string{"agy", "models"}) {
		t.Fatalf("CatalogProbeCommand = %#v, want agy models", antigravity.CatalogProbeCommand)
	}
	if antigravity.CatalogProbeParser != "agy-models" || !antigravity.CatalogProbeMayNetwork {
		t.Fatalf("catalog probe declaration = %#v, want agy-models network declared", antigravity)
	}
	grok, ok := runtimecap.LookupProvider("grok")
	if !ok {
		t.Fatal("LookupProvider(\"grok\") returned false")
	}
	if !reflect.DeepEqual(grok.CatalogProbeCommand, []string{"grok", "models"}) {
		t.Fatalf("CatalogProbeCommand = %#v, want grok models", grok.CatalogProbeCommand)
	}
	if grok.CatalogProbeParser != "grok-models" || !grok.CatalogProbeMayNetwork {
		t.Fatalf("grok catalog probe declaration = %#v, want grok-models with declared network", grok)
	}
}

func TestDefaultContractRepresentsExistingHosts(t *testing.T) {
	if got, want := runtimecap.HostNames(), []string{"claude-code", "codex-cli", "generic-local", "paseo-style"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("HostNames = %#v, want %#v", got, want)
	}

	for _, name := range []string{"codex-cli", "claude-code", "paseo-style", "generic-local"} {
		t.Run(name, func(t *testing.T) {
			host, ok := runtimecap.LookupHost(name)
			if !ok {
				t.Fatalf("LookupHost(%q) returned false", name)
			}
			if host.Name != name || host.InvocationStyle == "" || !host.PreservesStdout || !host.PreservesStderr || !host.SupportsJSONOutput {
				t.Fatalf("host capability = %#v", host)
			}
		})
	}
}

func TestContractReturnsCopies(t *testing.T) {
	contract := runtimecap.DefaultContract()
	contract.Providers[0].Name = "changed"
	contract.Providers[0].AuthProbeCommand = append(contract.Providers[0].AuthProbeCommand, "changed")
	contract.Providers[0].AuthArtifactPaths = append(contract.Providers[0].AuthArtifactPaths, "changed")
	contract.Providers[0].AuthEnvironmentNames = append(contract.Providers[0].AuthEnvironmentNames, "changed")
	contract.Providers[0].KnownLimitations = append(contract.Providers[0].KnownLimitations, "changed")
	contract.Hosts[0].Name = "changed"
	contract.Hosts[0].KnownLimitations = append(contract.Hosts[0].KnownLimitations, "changed")

	next := runtimecap.DefaultContract()
	if next.Providers[0].Name == "changed" || next.Hosts[0].Name == "changed" {
		t.Fatalf("DefaultContract leaked mutation: %#v", next)
	}
	if len(next.Hosts[0].KnownLimitations) > 0 && next.Hosts[0].KnownLimitations[len(next.Hosts[0].KnownLimitations)-1] == "changed" {
		t.Fatalf("DefaultContract leaked mutation: %#v", next)
	}
	for _, provider := range next.Providers {
		if provider.Name != "codex" {
			continue
		}
		if len(provider.AuthProbeCommand) != 3 || provider.AuthProbeCommand[len(provider.AuthProbeCommand)-1] == "changed" {
			t.Fatalf("DefaultContract leaked auth command mutation: %#v", provider)
		}
	}
}

func TestRequireProviderCapabilityReturnsActionableUnsupportedError(t *testing.T) {
	err := runtimecap.RequireProviderCapability("antigravity", runtimecap.ProviderReadOnly)
	if err == nil {
		t.Fatal("RequireProviderCapability returned nil error, want unsupported capability")
	}

	var unsupported runtimecap.UnsupportedCapabilityError
	if !errors.As(err, &unsupported) {
		t.Fatalf("error = %T %v, want UnsupportedCapabilityError", err, err)
	}
	if unsupported.Provider != "antigravity" || unsupported.Capability != runtimecap.ProviderReadOnly {
		t.Fatalf("unsupported error = %#v", unsupported)
	}
	for _, want := range []string{
		`provider "antigravity" does not support read-only`,
		"choose a supporting provider: claude, codex, gemini",
		"use antigravity only for write-mode worker dispatch",
		"read-only mode is not available or verified",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want substring %q", err.Error(), want)
		}
	}
}

func TestRequireProviderCapabilityPassesSupportedCapability(t *testing.T) {
	if err := runtimecap.RequireProviderCapability("codex", runtimecap.ProviderReadOnly); err != nil {
		t.Fatalf("RequireProviderCapability returned error: %v", err)
	}
}

func TestSmokeMatrixIncludesProviderHostRoleCompatibility(t *testing.T) {
	matrix := runtimecap.SmokeMatrix()
	if len(matrix) == 0 {
		t.Fatal("SmokeMatrix returned no entries")
	}

	var codexWorker, antigravityVerifier, claudeNested runtimecap.CompatibilityEntry
	for _, entry := range matrix {
		switch {
		case entry.Provider == "codex" && entry.Host == "codex-cli" && entry.Role == runtimecap.RoleWorker:
			codexWorker = entry
		case entry.Provider == "antigravity" && entry.Host == "claude-code" && entry.Role == runtimecap.RoleVerifier:
			antigravityVerifier = entry
		case entry.Provider == "claude" && entry.Host == "claude-code" && entry.Role == runtimecap.RoleNestedSubagents:
			claudeNested = entry
		}
	}
	if codexWorker.Support != runtimecap.SupportSupported || codexWorker.Code != "supported" {
		t.Fatalf("codex worker entry = %#v, want supported", codexWorker)
	}
	if antigravityVerifier.Support != runtimecap.SupportUnsupported || antigravityVerifier.Code != "unsupported_read_only_mode" {
		t.Fatalf("antigravity verifier entry = %#v, want unsupported read-only", antigravityVerifier)
	}
	if claudeNested.Support != runtimecap.SupportSupported || claudeNested.Code != "supported" {
		t.Fatalf("claude nested entry = %#v, want supported", claudeNested)
	}
}

func TestEvaluateCompatibilityMarksGenericAndGeminiExperimental(t *testing.T) {
	entry := runtimecap.EvaluateCompatibility("gemini", "generic-local", runtimecap.RoleWorker)
	if entry.Support != runtimecap.SupportExperimental || entry.Code != "experimental" {
		t.Fatalf("entry = %#v, want experimental", entry)
	}
}

func TestInvariantViolationsRejectInvalidContract(t *testing.T) {
	contract := runtimecap.Contract{
		Providers: []runtimecap.ProviderRuntime{
			{Name: "custom"},
			{Name: "custom", Executable: "custom"},
			{Name: "bad/path", Executable: "bad/path", AuthUnsupportedReason: "unsupported"},
			{Name: "network", Executable: "network", MayNetwork: true, AuthUnsupportedReason: "unsupported"},
			{Name: "catalog", Executable: "catalog", CatalogProbeMayNetwork: true, AuthUnsupportedReason: "unsupported"},
		},
		Hosts: []runtimecap.HostRuntime{
			{Name: "host"},
		},
	}
	violations := contract.InvariantViolations()
	for _, want := range []string{
		`provider "custom" executable is empty`,
		`provider "custom" is duplicated`,
		`provider "bad/path" name contains a path separator`,
		`provider "bad/path" executable "bad/path" must be a command name, not a path`,
		`provider "network" declares network auth probing without an auth probe command`,
		`provider "catalog" declares network catalog probing without a catalog probe command`,
		`host "host" invocation style is empty`,
		`host "host" must preserve stdout`,
		`host "host" must preserve stderr`,
	} {
		found := false
		for _, violation := range violations {
			if violation == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("violations = %#v, missing %q", violations, want)
		}
	}
}
