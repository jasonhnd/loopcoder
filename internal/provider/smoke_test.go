package provider

import (
	"testing"

	"github.com/jasonhnd/loopcoder/internal/runtimecap"
)

func TestFakeProviderSmokeMatrixPassesSupportedProvider(t *testing.T) {
	contract := fakeSmokeContract(runtimecap.ProviderRuntime{
		Name:                "fake",
		Executable:          "fake",
		ReadOnly:            true,
		NestedSubagents:     true,
		JSONOutput:          true,
		MCPConfig:           true,
		Cancellation:        true,
		TokenUsageReporting: true,
	})

	entry := Check(contract, "fake", "fake-host", runtimecap.RoleVerifier)
	if entry.Support != runtimecap.SupportSupported || entry.Code != "supported" || len(entry.MissingCapabilities) != 0 {
		t.Fatalf("entry = %#v, want supported verifier smoke", entry)
	}
}

func TestFakeProviderSmokeMatrixDistinguishesUnsupportedModes(t *testing.T) {
	contract := fakeSmokeContract(runtimecap.ProviderRuntime{
		Name:         "fake",
		Executable:   "fake",
		Cancellation: true,
	})

	verifier := Check(contract, "fake", "fake-host", runtimecap.RoleVerifier)
	if verifier.Support != runtimecap.SupportUnsupported || verifier.Code != "unsupported_read_only_mode" {
		t.Fatalf("verifier entry = %#v, want unsupported read-only mode", verifier)
	}

	nested := Check(contract, "fake", "fake-host", runtimecap.RoleNestedSubagents)
	if nested.Support != runtimecap.SupportUnsupported || nested.Code != "unsupported_nested_agents" {
		t.Fatalf("nested entry = %#v, want unsupported nested agents", nested)
	}
}

func fakeSmokeContract(provider runtimecap.ProviderRuntime) runtimecap.Contract {
	return runtimecap.Contract{
		Providers: []runtimecap.ProviderRuntime{provider},
		Hosts: []runtimecap.HostRuntime{{
			Name:               "fake-host",
			InvocationStyle:    "fake host invokes loopcoder as a local subprocess",
			PreservesStdout:    true,
			PreservesStderr:    true,
			SupportsJSONOutput: true,
			SupportsTimeouts:   true,
			SupportsCancel:     true,
		}},
	}
}
