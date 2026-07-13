package runtimecap_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/runtimecap"
)

func TestNegotiateHostFixturesAreDeterministic(t *testing.T) {
	tests := []struct {
		name    string
		profile string
		outcome runtimecap.HostNegotiationOutcome
	}{
		{name: "codex", profile: "codex-cli", outcome: runtimecap.HostNegotiationSupported},
		{name: "claude", profile: "claude-code", outcome: runtimecap.HostNegotiationSupported},
		{name: "generic", profile: "generic-local", outcome: runtimecap.HostNegotiationSupported},
		{name: "paseo", profile: "paseo-style", outcome: runtimecap.HostNegotiationSupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, ok := runtimecap.LookupHost(tt.profile)
			if !ok {
				t.Fatalf("LookupHost(%q) returned false", tt.profile)
			}
			request := runtimecap.HostNegotiationRequest{
				SchemaVersion: runtimecap.HostNegotiationSchemaVersion,
				Host: runtimecap.HostProfileRecord{
					Name:   host.Name,
					Source: "fixture",
				},
				Capabilities: runtimecap.HostCapabilityDeclarations(host),
			}
			first := runtimecap.NegotiateHost(request)
			second := runtimecap.NegotiateHost(request)
			firstJSON := marshalNegotiation(t, first)
			secondJSON := marshalNegotiation(t, second)
			if firstJSON != secondJSON {
				t.Fatalf("negotiation is not deterministic:\nfirst=%s\nsecond=%s", firstJSON, secondJSON)
			}
			if first.SchemaVersion != runtimecap.HostNegotiationSchemaVersion {
				t.Fatalf("schema_version = %q", first.SchemaVersion)
			}
			if first.Profile.Name != tt.profile || first.Compatibility.Outcome != tt.outcome || first.Compatibility.Code != "supported" {
				t.Fatalf("contract = %#v", first)
			}
			if first.Outputs.Format != "json" || !first.Outputs.StableJSON || !first.Outputs.CredentialBlind || first.Outputs.IncludesLocalPaths {
				t.Fatalf("outputs = %#v, want stable credential-blind JSON without local paths", first.Outputs)
			}
			if first.Streaming.Stdout != runtimecap.HostCapabilitySupported || first.Streaming.Stderr != runtimecap.HostCapabilitySupported {
				t.Fatalf("streaming = %#v, want supported stdout/stderr", first.Streaming)
			}
		})
	}
}

func TestNegotiateHostSchemaVersionRules(t *testing.T) {
	host, ok := runtimecap.LookupHost("codex-cli")
	if !ok {
		t.Fatal("LookupHost(\"codex-cli\") returned false")
	}
	base := runtimecap.HostNegotiationRequest{
		Host:         runtimecap.HostProfileRecord{Name: "codex-cli", Source: "fixture"},
		Capabilities: runtimecap.HostCapabilityDeclarations(host),
	}

	tests := []struct {
		name     string
		mutate   func(*runtimecap.HostNegotiationRequest)
		outcome  runtimecap.HostNegotiationOutcome
		code     string
		selected string
	}{
		{
			name: "current",
			mutate: func(req *runtimecap.HostNegotiationRequest) {
				req.SchemaVersion = runtimecap.HostNegotiationSchemaVersion
			},
			outcome:  runtimecap.HostNegotiationSupported,
			code:     "supported",
			selected: runtimecap.HostNegotiationSchemaVersion,
		},
		{
			name: "newer with current fallback",
			mutate: func(req *runtimecap.HostNegotiationRequest) {
				req.SchemaVersion = "loopcoder.host_negotiation.v2"
				req.SupportedSchemaVersions = []string{"loopcoder.host_negotiation.v2", runtimecap.HostNegotiationSchemaVersion}
			},
			outcome:  runtimecap.HostNegotiationSupported,
			code:     "supported",
			selected: runtimecap.HostNegotiationSchemaVersion,
		},
		{
			name: "newer without current fallback",
			mutate: func(req *runtimecap.HostNegotiationRequest) {
				req.SchemaVersion = "loopcoder.host_negotiation.v2"
				req.SupportedSchemaVersions = []string{"loopcoder.host_negotiation.v2"}
			},
			outcome:  runtimecap.HostNegotiationIncompatible,
			code:     runtimecap.ErrUnsupportedHostSchemaVersion,
			selected: "",
		},
		{
			name: "older without current fallback",
			mutate: func(req *runtimecap.HostNegotiationRequest) {
				req.SchemaVersion = "loopcoder.host_negotiation.v0"
			},
			outcome:  runtimecap.HostNegotiationIncompatible,
			code:     runtimecap.ErrUnsupportedHostSchemaVersion,
			selected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := base
			tt.mutate(&req)
			contract := runtimecap.NegotiateHost(req)
			if contract.Compatibility.Outcome != tt.outcome || contract.Compatibility.Code != tt.code || contract.Compatibility.SelectedSchema != tt.selected {
				t.Fatalf("compatibility = %#v", contract.Compatibility)
			}
		})
	}
}

func TestNegotiateHostRejectsUnsupportedFeatures(t *testing.T) {
	host, ok := runtimecap.LookupHost("claude-code")
	if !ok {
		t.Fatal("LookupHost(\"claude-code\") returned false")
	}
	contract := runtimecap.NegotiateHost(runtimecap.HostNegotiationRequest{
		SchemaVersion:     runtimecap.HostNegotiationSchemaVersion,
		RequestedFeatures: []runtimecap.HostFeature{"future-stream-v2"},
		Host:              runtimecap.HostProfileRecord{Name: "claude-code", Source: "fixture"},
		Capabilities:      runtimecap.HostCapabilityDeclarations(host),
	})
	if contract.Compatibility.Outcome != runtimecap.HostNegotiationUnsupported || contract.Compatibility.Code != runtimecap.ErrUnsupportedHostFeature {
		t.Fatalf("compatibility = %#v", contract.Compatibility)
	}
	if len(contract.Compatibility.UnsupportedFeatures) != 1 || contract.Compatibility.UnsupportedFeatures[0] != "future-stream-v2" {
		t.Fatalf("unsupported features = %#v", contract.Compatibility.UnsupportedFeatures)
	}
}

func TestNegotiateHostMissingAndPartialMetadata(t *testing.T) {
	missing := runtimecap.NegotiateHost(runtimecap.HostNegotiationRequest{
		SchemaVersion: runtimecap.HostNegotiationSchemaVersion,
		Host:          runtimecap.HostProfileRecord{Name: "custom", Source: "fixture"},
	})
	if missing.Compatibility.Outcome != runtimecap.HostNegotiationIncompatible || missing.Compatibility.Code != runtimecap.ErrMissingHostMetadata {
		t.Fatalf("missing compatibility = %#v", missing.Compatibility)
	}

	partial := runtimecap.NegotiateHost(runtimecap.HostNegotiationRequest{
		SchemaVersion: runtimecap.HostNegotiationSchemaVersion,
		Host:          runtimecap.HostProfileRecord{Name: "custom", Source: "fixture"},
		Capabilities: []runtimecap.HostCapabilityDeclaration{
			{Capability: runtimecap.HostStdout, Support: runtimecap.HostCapabilitySupported},
		},
	})
	if partial.Compatibility.Outcome != runtimecap.HostNegotiationUnsupported || partial.Compatibility.Code != runtimecap.ErrPartialHostMetadata {
		t.Fatalf("partial compatibility = %#v", partial.Compatibility)
	}
	for _, want := range []runtimecap.HostCapability{runtimecap.HostJSONOutput, runtimecap.HostLocalSubprocess, runtimecap.HostStderr} {
		if !containsHostCapability(partial.Compatibility.MissingCapabilities, want) {
			t.Fatalf("missing capabilities = %#v, want %s", partial.Compatibility.MissingCapabilities, want)
		}
	}
}

func TestNegotiateHostRedactsSecretAndLocalPathMetadata(t *testing.T) {
	contract := runtimecap.NegotiateHost(runtimecap.HostNegotiationRequest{
		SchemaVersion: runtimecap.HostNegotiationSchemaVersion,
		RequestedFeatures: []runtimecap.HostFeature{
			"/Users/alice/future-feature",
		},
		Host: runtimecap.HostProfileRecord{
			Name:   "/Users/alice/.codex/sk-secret",
			Source: "ghp_secret_token",
		},
		Capabilities: []runtimecap.HostCapabilityDeclaration{
			{Capability: runtimecap.HostLocalSubprocess, Support: runtimecap.HostCapabilitySupported, Source: "/Users/alice/.config/host"},
			{Capability: runtimecap.HostStdout, Support: runtimecap.HostCapabilitySupported},
			{Capability: runtimecap.HostStderr, Support: runtimecap.HostCapabilitySupported},
			{Capability: runtimecap.HostJSONOutput, Support: runtimecap.HostCapabilitySupported},
			{Capability: "/Users/alice/private-capability", Support: runtimecap.HostCapabilitySupported},
		},
	})
	payload := marshalNegotiation(t, contract)
	for _, forbidden := range []string{"/Users", ".codex", "sk-secret", "ghp_secret_token", "future-feature", "private-capability"} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("negotiation leaked %q in JSON:\n%s", forbidden, payload)
		}
	}
	if contract.Profile.Name != "unspecified" || contract.Profile.Source != "unspecified" {
		t.Fatalf("profile = %#v, want redacted diagnostic values", contract.Profile)
	}
	if len(contract.Compatibility.UnsupportedFeatures) != 1 || contract.Compatibility.UnsupportedFeatures[0] != "redacted-unsupported-feature" {
		t.Fatalf("unsupported features = %#v, want redacted unsupported feature", contract.Compatibility.UnsupportedFeatures)
	}
}

func marshalNegotiation(t *testing.T, contract runtimecap.HostNegotiation) string {
	t.Helper()
	data, err := json.Marshal(contract)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(data)
}

func containsHostCapability(capabilities []runtimecap.HostCapability, want runtimecap.HostCapability) bool {
	for _, capability := range capabilities {
		if capability == want {
			return true
		}
	}
	return false
}
