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
	if got, want := runtimecap.ProviderNames(), []string{"antigravity", "claude", "codex", "gemini"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ProviderNames = %#v, want %#v", got, want)
	}

	tests := []struct {
		provider   string
		executable string
		readOnly   bool
		mcp        bool
		json       bool
		usage      bool
	}{
		{provider: "codex", executable: "codex", readOnly: true, mcp: true, json: true, usage: true},
		{provider: "claude", executable: "claude", readOnly: true, mcp: true, json: true, usage: true},
		{provider: "gemini", executable: "gemini", readOnly: true, mcp: true, json: true, usage: true},
		{provider: "antigravity", executable: "agy", readOnly: false, mcp: false, json: false, usage: false},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			provider, ok := runtimecap.LookupProvider(tt.provider)
			if !ok {
				t.Fatalf("LookupProvider(%q) returned false", tt.provider)
			}
			if provider.Executable != tt.executable ||
				provider.Supports(runtimecap.ProviderReadOnly) != tt.readOnly ||
				provider.Supports(runtimecap.ProviderMCPConfig) != tt.mcp ||
				provider.Supports(runtimecap.ProviderJSONOutput) != tt.json ||
				provider.Supports(runtimecap.ProviderTokenUsage) != tt.usage {
				t.Fatalf("provider capability = %#v", provider)
			}
		})
	}
}

func TestContractReturnsCopies(t *testing.T) {
	contract := runtimecap.DefaultContract()
	contract.Providers[0].Name = "changed"
	contract.Providers[0].KnownLimitations = append(contract.Providers[0].KnownLimitations, "changed")
	contract.Hosts[0].Name = "changed"

	next := runtimecap.DefaultContract()
	if next.Providers[0].Name == "changed" || next.Hosts[0].Name == "changed" {
		t.Fatalf("DefaultContract leaked mutation: %#v", next)
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

func TestInvariantViolationsRejectInvalidContract(t *testing.T) {
	contract := runtimecap.Contract{
		Providers: []runtimecap.ProviderRuntime{
			{Name: "custom"},
			{Name: "custom", Executable: "custom"},
		},
		Hosts: []runtimecap.HostRuntime{
			{Name: "host"},
		},
	}
	violations := contract.InvariantViolations()
	for _, want := range []string{
		`provider "custom" executable is empty`,
		`provider "custom" is duplicated`,
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
