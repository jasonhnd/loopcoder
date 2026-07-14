package runtimecap_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"sync"
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

func TestNegotiateHostProgressTransportModes(t *testing.T) {
	tests := []struct {
		name         string
		capabilities []runtimecap.HostCapabilityDeclaration
		origin       runtimecap.HostRunOriginDeclaration
		contract     string
		ackPolicy    string
		wakeCode     string
		ackCode      string
	}{
		{
			name:         "acknowledged streaming",
			capabilities: progressCapabilities(runtimecap.HostCallbacks, runtimecap.HostWakeUp, runtimecap.HostAcknowledgment),
			origin:       originDeclaration("host-session-ack", nil),
			contract:     runtimecap.HostProgressAcknowledgedStreaming,
			ackPolicy:    runtimecap.HostProgressAckRequired,
			wakeCode:     runtimecap.HostStageEvidenceRequired,
			ackCode:      runtimecap.HostStageEvidenceRequired,
		},
		{
			name:         "unacknowledged streaming",
			capabilities: progressCapabilities(runtimecap.HostCallbacks, runtimecap.HostWakeUp),
			origin:       originDeclaration("host-session-unack", nil),
			contract:     runtimecap.HostProgressUnacknowledgedStreaming,
			ackPolicy:    runtimecap.HostProgressAckNone,
			wakeCode:     runtimecap.HostStageEvidenceRequired,
			ackCode:      runtimecap.HostStageUnsupported,
		},
		{
			name:         "poll follow only",
			capabilities: progressCapabilities(runtimecap.HostDurablePolling, runtimecap.HostResumableFollow),
			contract:     runtimecap.HostProgressDurableFollowPoll,
			ackPolicy:    runtimecap.HostProgressAckNone,
			wakeCode:     runtimecap.HostStageUnsupported,
			ackCode:      runtimecap.HostStageUnsupported,
		},
		{
			name:         "known origin without wake",
			capabilities: progressCapabilities(),
			origin:       originDeclaration("known-origin-no-wake", nil),
			contract:     runtimecap.HostProgressKnownOriginReplay,
			ackPolicy:    runtimecap.HostProgressAckNone,
			wakeCode:     runtimecap.HostStageUnsupported,
			ackCode:      runtimecap.HostStageUnsupported,
		},
		{
			name:         "generic unknown host",
			capabilities: progressCapabilities(),
			contract:     runtimecap.HostProgressNextInvocationReplay,
			ackPolicy:    runtimecap.HostProgressAckNone,
			wakeCode:     runtimecap.HostStageUnsupported,
			ackCode:      runtimecap.HostStageUnsupported,
		},
		{
			name:         "synthetic future host declaration",
			capabilities: progressCapabilities(runtimecap.HostCallbacks, runtimecap.HostWakeUp, runtimecap.HostAcknowledgment, runtimecap.HostDetachedSteering, runtimecap.HostDetachedCancellation),
			origin:       originDeclaration("future-host-origin", map[string]string{"schema": "future"}),
			contract:     runtimecap.HostProgressAcknowledgedStreaming,
			ackPolicy:    runtimecap.HostProgressAckRequired,
			wakeCode:     runtimecap.HostStageEvidenceRequired,
			ackCode:      runtimecap.HostStageEvidenceRequired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			contract := runtimecap.NegotiateHost(runtimecap.HostNegotiationRequest{
				SchemaVersion: runtimecap.HostNegotiationSchemaVersion,
				Host:          runtimecap.HostProfileRecord{Name: tt.name, Source: "fixture-declaration"},
				Capabilities:  tt.capabilities,
				ProgressLimits: runtimecap.HostProgressLimitDeclaration{
					MaxPayloadBytes:      32768,
					MaxEnvelopeBytes:     65536,
					MaxReceiptsPerMinute: 120,
					MaxOutstanding:       8,
				},
				Origin: runtimecap.HostRunOriginBindingRequest{
					ProjectID:     "proj_transport",
					DeliveryRunID: "run_transport",
					CorrelationID: "corr_transport",
					Origin:        tt.origin,
				},
			})
			if contract.Progress.TransportContract != tt.contract || contract.Progress.AckPolicy != tt.ackPolicy {
				t.Fatalf("progress = %#v", contract.Progress)
			}
			if got := stageCode(contract.Progress.Stages, runtimecap.HostProgressStageWakeUp); got != tt.wakeCode {
				t.Fatalf("wake stage code = %q, want %q; stages=%#v", got, tt.wakeCode, contract.Progress.Stages)
			}
			if got := stageCode(contract.Progress.Stages, runtimecap.HostProgressStageAcknowledgment); got != tt.ackCode {
				t.Fatalf("ack stage code = %q, want %q; stages=%#v", got, tt.ackCode, contract.Progress.Stages)
			}
			for _, stage := range contract.Progress.Stages {
				if stage.Code == "accepted" || stage.Code == "visible" || stage.Code == "acknowledged" {
					t.Fatalf("negotiation claimed delivery success without evidence: %#v", stage)
				}
			}
		})
	}
}

func TestNegotiateHostProgressDoesNotInferActiveSupportFromProfile(t *testing.T) {
	contract := runtimecap.NegotiateHost(runtimecap.HostNegotiationRequest{
		SchemaVersion: runtimecap.HostNegotiationSchemaVersion,
		Host:          runtimecap.HostProfileRecord{Name: "claude-code", Source: "detection"},
		Capabilities:  progressCapabilities(),
	})
	if contract.Progress.TransportContract != runtimecap.HostProgressNextInvocationReplay {
		t.Fatalf("contract = %q, want profile-only fallback replay", contract.Progress.TransportContract)
	}
	for _, capability := range []runtimecap.HostCapability{runtimecap.HostCallbacks, runtimecap.HostWakeUp, runtimecap.HostAcknowledgment} {
		if got := capabilitySupport(contract.Capabilities, capability); got != runtimecap.HostCapabilityUnknown {
			t.Fatalf("%s support = %q, want unknown without declaration evidence", capability, got)
		}
	}
}

func TestCodexHostDeclaresOnlyDocumentedProgressCapabilities(t *testing.T) {
	host, ok := runtimecap.LookupHost("codex-cli")
	if !ok {
		t.Fatal("LookupHost(codex-cli) returned false")
	}
	contract := runtimecap.NegotiateHost(runtimecap.HostNegotiationRequest{
		SchemaVersion: runtimecap.HostNegotiationSchemaVersion,
		Host:          runtimecap.HostProfileRecord{Name: host.Name, Source: "fixture"},
		Capabilities:  runtimecap.HostCapabilityDeclarations(host),
	})
	if contract.Progress.TransportContract != runtimecap.HostProgressDurableFollowPoll || contract.Progress.AckPolicy != runtimecap.HostProgressAckNone {
		t.Fatalf("Codex progress = %#v, want durable follow/poll without ack", contract.Progress)
	}
	for capability, want := range map[runtimecap.HostCapability]runtimecap.HostCapabilitySupport{
		runtimecap.HostDurablePolling:        runtimecap.HostCapabilitySupported,
		runtimecap.HostResumableFollow:       runtimecap.HostCapabilitySupported,
		runtimecap.HostDetachedSteering:      runtimecap.HostCapabilityUnknown,
		runtimecap.HostDetachedCancellation:  runtimecap.HostCapabilitySupported,
		runtimecap.HostCallbacks:             runtimecap.HostCapabilityUnsupported,
		runtimecap.HostWakeUp:                runtimecap.HostCapabilityUnsupported,
		runtimecap.HostAcknowledgment:        runtimecap.HostCapabilityUnsupported,
		runtimecap.HostManagedBackgroundWork: runtimecap.HostCapabilityUnsupported,
	} {
		if got := capabilitySupport(contract.Capabilities, capability); got != want {
			t.Fatalf("%s support = %q, want %q", capability, got, want)
		}
	}
	if got := stageCode(contract.Progress.Stages, runtimecap.HostProgressStageWakeUp); got != runtimecap.HostStageUnsupported {
		t.Fatalf("wake stage = %q, want unsupported", got)
	}
	if got := stageCode(contract.Progress.Stages, runtimecap.HostProgressStageAcknowledgment); got != runtimecap.HostStageUnsupported {
		t.Fatalf("ack stage = %q, want unsupported", got)
	}
}

func TestClaudeCodeHostDeclaresOnlyDocumentedProgressCapabilities(t *testing.T) {
	host, ok := runtimecap.LookupHost("claude-code")
	if !ok {
		t.Fatal("LookupHost(claude-code) returned false")
	}
	contract := runtimecap.NegotiateHost(runtimecap.HostNegotiationRequest{
		SchemaVersion: runtimecap.HostNegotiationSchemaVersion,
		Host:          runtimecap.HostProfileRecord{Name: host.Name, Source: "fixture"},
		Capabilities:  runtimecap.HostCapabilityDeclarations(host),
	})
	if contract.Progress.TransportContract != runtimecap.HostProgressDurableFollowPoll || contract.Progress.AckPolicy != runtimecap.HostProgressAckNone {
		t.Fatalf("Claude progress = %#v, want durable follow/poll without ack", contract.Progress)
	}
	for capability, want := range map[runtimecap.HostCapability]runtimecap.HostCapabilitySupport{
		runtimecap.HostDurablePolling:        runtimecap.HostCapabilitySupported,
		runtimecap.HostResumableFollow:       runtimecap.HostCapabilitySupported,
		runtimecap.HostDetachedSteering:      runtimecap.HostCapabilityUnknown,
		runtimecap.HostDetachedCancellation:  runtimecap.HostCapabilityUnknown,
		runtimecap.HostCallbacks:             runtimecap.HostCapabilityUnsupported,
		runtimecap.HostWakeUp:                runtimecap.HostCapabilityUnsupported,
		runtimecap.HostAcknowledgment:        runtimecap.HostCapabilityUnsupported,
		runtimecap.HostManagedBackgroundWork: runtimecap.HostCapabilityUnsupported,
		runtimecap.HostHooks:                 runtimecap.HostCapabilitySupported,
	} {
		if got := capabilitySupport(contract.Capabilities, capability); got != want {
			t.Fatalf("%s support = %q, want %q", capability, got, want)
		}
	}
	for _, stage := range []runtimecap.HostProgressStage{runtimecap.HostProgressStageWakeUp, runtimecap.HostProgressStageUserVisibility, runtimecap.HostProgressStageAcknowledgment} {
		if got := stageCode(contract.Progress.Stages, stage); got != runtimecap.HostStageUnsupported {
			t.Fatalf("%s stage = %q, want unsupported", stage, got)
		}
	}
}

func TestRealCodexCLISmokeDocumentedCapabilities(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("real Codex CLI smoke is limited to macOS Apple Silicon")
	}
	if testing.Short() {
		t.Skip("real Codex CLI smoke is opt-in")
	}
	if _, ok := os.LookupEnv("LOOPCODER_REAL_CODEX_SMOKE"); !ok {
		t.Skip("set LOOPCODER_REAL_CODEX_SMOKE=1 to run the credential-free Codex CLI smoke")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skipf("codex CLI not on PATH: %v", err)
	}
	out, err := exec.Command("codex", "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("codex --version failed: %v\n%s", err, string(out))
	}
	host, _ := runtimecap.LookupHost("codex-cli")
	contract := runtimecap.NegotiateHost(runtimecap.HostNegotiationRequest{
		SchemaVersion: runtimecap.HostNegotiationSchemaVersion,
		Host:          runtimecap.HostProfileRecord{Name: host.Name, Source: "real-smoke"},
		Capabilities:  runtimecap.HostCapabilityDeclarations(host),
	})
	if contract.Progress.TransportContract != runtimecap.HostProgressDurableFollowPoll ||
		capabilitySupport(contract.Capabilities, runtimecap.HostCallbacks) != runtimecap.HostCapabilityUnsupported ||
		capabilitySupport(contract.Capabilities, runtimecap.HostWakeUp) != runtimecap.HostCapabilityUnsupported ||
		capabilitySupport(contract.Capabilities, runtimecap.HostAcknowledgment) != runtimecap.HostCapabilityUnsupported {
		t.Fatalf("real Codex smoke declared unsupported callback/wake/ack incorrectly: %#v", contract.Progress)
	}
}

func TestRealClaudeCodeSmokeDocumentedCapabilities(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("real Claude Code smoke is limited to macOS Apple Silicon")
	}
	if testing.Short() {
		t.Skip("real Claude Code smoke is opt-in")
	}
	if _, ok := os.LookupEnv("LOOPCODER_REAL_CLAUDE_CODE_SMOKE"); !ok {
		t.Skip("set LOOPCODER_REAL_CLAUDE_CODE_SMOKE=1 to run the credential-free Claude Code smoke")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("claude CLI not on PATH: %v", err)
	}
	out, err := exec.Command("claude", "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("claude --version failed: %v\n%s", err, string(out))
	}
	host, _ := runtimecap.LookupHost("claude-code")
	contract := runtimecap.NegotiateHost(runtimecap.HostNegotiationRequest{
		SchemaVersion: runtimecap.HostNegotiationSchemaVersion,
		Host:          runtimecap.HostProfileRecord{Name: host.Name, Source: "real-smoke"},
		Capabilities:  runtimecap.HostCapabilityDeclarations(host),
	})
	if contract.Progress.TransportContract != runtimecap.HostProgressDurableFollowPoll ||
		capabilitySupport(contract.Capabilities, runtimecap.HostCallbacks) != runtimecap.HostCapabilityUnsupported ||
		capabilitySupport(contract.Capabilities, runtimecap.HostWakeUp) != runtimecap.HostCapabilityUnsupported ||
		capabilitySupport(contract.Capabilities, runtimecap.HostAcknowledgment) != runtimecap.HostCapabilityUnsupported {
		t.Fatalf("real Claude Code smoke declared unsupported callback/wake/ack incorrectly: %#v", contract.Progress)
	}
}

func TestNegotiateHostProviderModelIndependence(t *testing.T) {
	hostRequest := runtimecap.HostNegotiationRequest{
		SchemaVersion: runtimecap.HostNegotiationSchemaVersion,
		Host:          runtimecap.HostProfileRecord{Name: "synthetic-host", Source: "fixture"},
		Capabilities:  progressCapabilities(runtimecap.HostCallbacks, runtimecap.HostWakeUp, runtimecap.HostAcknowledgment),
	}
	baseline := marshalNegotiation(t, runtimecap.NegotiateHost(hostRequest))

	type providerCase struct {
		provider string
		model    string
		invoked  *int
	}
	providers := []providerCase{
		{provider: "codex", model: "gpt-5.5", invoked: new(int)},
		{provider: "claude", model: "claude-opus-4.5", invoked: new(int)},
		{provider: "gemini", model: "gemini-3.1-pro", invoked: new(int)},
		{provider: "grok", model: "grok-build", invoked: new(int)},
		{provider: "synthetic", model: "future-model", invoked: new(int)},
	}
	for _, provider := range providers {
		t.Run(provider.provider+"/"+provider.model, func(t *testing.T) {
			got := marshalNegotiation(t, runtimecap.NegotiateHost(hostRequest))
			if got != baseline {
				t.Fatalf("host negotiation changed for provider %s model %s", provider.provider, provider.model)
			}
			if *provider.invoked != 0 {
				t.Fatalf("provider invocation count = %d, want zero", *provider.invoked)
			}
		})
	}
}

func TestBindHostRunOriginScopeAndRedaction(t *testing.T) {
	secret := "sk-" + "origin-secret-canary"
	base := runtimecap.HostRunOriginBindingRequest{
		ProjectID:     "proj_origin",
		DeliveryRunID: "run_origin",
		CorrelationID: "corr_origin",
		Origin: runtimecap.HostRunOriginDeclaration{
			SchemaVersion: runtimecap.HostRunOriginSchemaVersion,
			Kind:          "callback-session",
			OpaqueID:      "opaque-" + secret,
			Metadata: map[string]string{
				"cwd":   "/Users/alice/private/repo",
				"token": secret,
				"label": "primary",
			},
		},
	}
	first := runtimecap.BindHostRunOrigin(base)
	second := runtimecap.BindHostRunOrigin(base)
	if !first.Bound || first.Code != runtimecap.HostOriginBound || first.BindingID == "" {
		t.Fatalf("first binding = %#v", first)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("origin binding is not restart-stable:\nfirst=%#v\nsecond=%#v", first, second)
	}
	if strings.Contains(mustJSON(t, first), secret) || strings.Contains(mustJSON(t, first), "/Users") {
		t.Fatalf("origin binding leaked secret/path: %s", mustJSON(t, first))
	}

	otherProject := base
	otherProject.ProjectID = "proj_other"
	otherRun := base
	otherRun.DeliveryRunID = "run_other"
	otherCorrelation := base
	otherCorrelation.CorrelationID = "corr_other"
	for name, req := range map[string]runtimecap.HostRunOriginBindingRequest{
		"project":     otherProject,
		"run":         otherRun,
		"correlation": otherCorrelation,
	} {
		t.Run("scope "+name, func(t *testing.T) {
			got := runtimecap.BindHostRunOrigin(req)
			if got.BindingID == first.BindingID {
				t.Fatalf("binding replayed across %s scope: %s", name, got.BindingID)
			}
		})
	}
}

func TestBindHostRunOriginRejectsInvalidMissingOversizedAndFutureSchemas(t *testing.T) {
	tests := []struct {
		name string
		req  runtimecap.HostRunOriginBindingRequest
		code string
	}{
		{
			name: "missing optional origin",
			req: runtimecap.HostRunOriginBindingRequest{
				ProjectID:     "proj_origin",
				DeliveryRunID: "run_origin",
				CorrelationID: "corr_origin",
			},
			code: runtimecap.HostOriginAbsent,
		},
		{
			name: "missing scope",
			req: runtimecap.HostRunOriginBindingRequest{
				ProjectID: "proj_origin",
				Origin:    originDeclaration("opaque", nil),
			},
			code: runtimecap.ErrInvalidHostOriginScope,
		},
		{
			name: "future schema without fallback",
			req: runtimecap.HostRunOriginBindingRequest{
				ProjectID:     "proj_origin",
				DeliveryRunID: "run_origin",
				CorrelationID: "corr_origin",
				Origin: runtimecap.HostRunOriginDeclaration{
					SchemaVersion:           "loopcoder.host_run_origin.v2",
					SupportedSchemaVersions: []string{"loopcoder.host_run_origin.v2"},
					Kind:                    "callback-session",
					OpaqueID:                "opaque",
				},
			},
			code: runtimecap.ErrUnsupportedHostOriginSchemaVersion,
		},
		{
			name: "oversized metadata",
			req: runtimecap.HostRunOriginBindingRequest{
				ProjectID:     "proj_origin",
				DeliveryRunID: "run_origin",
				CorrelationID: "corr_origin",
				Origin:        originDeclaration("opaque", map[string]string{"huge": strings.Repeat("x", 5000)}),
			},
			code: runtimecap.ErrHostOriginMetadataTooLarge,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runtimecap.BindHostRunOrigin(tt.req)
			if got.Code != tt.code {
				t.Fatalf("binding code = %q, want %q; binding=%#v", got.Code, tt.code, got)
			}
			if tt.code != runtimecap.HostOriginAbsent && got.Bound {
				t.Fatalf("binding unexpectedly bound: %#v", got)
			}
		})
	}
}

func TestNegotiateHostConcurrentCanonicalizationIsStable(t *testing.T) {
	requests := make([]runtimecap.HostNegotiationRequest, 256)
	for i := range requests {
		requests[i] = runtimecap.HostNegotiationRequest{
			SchemaVersion: runtimecap.HostNegotiationSchemaVersion,
			Host:          runtimecap.HostProfileRecord{Name: fmt.Sprintf("host-%03d", 255-i), Source: "race-fixture"},
			Capabilities: append(progressCapabilities(runtimecap.HostDurablePolling, runtimecap.HostResumableFollow),
				runtimecap.HostCapabilityDeclaration{Capability: runtimecap.HostCallbacks, Support: runtimecap.HostCapabilityUnknown, Source: "fixture"}),
			Origin: runtimecap.HostRunOriginBindingRequest{
				ProjectID:     "proj_race",
				DeliveryRunID: "run_race",
				CorrelationID: fmt.Sprintf("corr_%03d", i%8),
				Origin:        originDeclaration(fmt.Sprintf("opaque-%03d", i%8), map[string]string{"k": fmt.Sprintf("%03d", i%8)}),
			},
		}
	}
	baseline := make([]string, len(requests))
	for i, req := range requests {
		baseline[i] = marshalNegotiation(t, runtimecap.NegotiateHost(req))
	}

	var wg sync.WaitGroup
	errs := make(chan string, len(requests))
	for i, req := range requests {
		wg.Add(1)
		go func(i int, req runtimecap.HostNegotiationRequest) {
			defer wg.Done()
			if got := marshalNegotiation(t, runtimecap.NegotiateHost(req)); got != baseline[i] {
				errs <- fmt.Sprintf("request %d changed", i)
			}
		}(i, req)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
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

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
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

func progressCapabilities(extra ...runtimecap.HostCapability) []runtimecap.HostCapabilityDeclaration {
	base := []runtimecap.HostCapabilityDeclaration{
		{Capability: runtimecap.HostLocalSubprocess, Support: runtimecap.HostCapabilitySupported, Source: "fixture"},
		{Capability: runtimecap.HostStdout, Support: runtimecap.HostCapabilitySupported, Source: "fixture"},
		{Capability: runtimecap.HostStderr, Support: runtimecap.HostCapabilitySupported, Source: "fixture"},
		{Capability: runtimecap.HostJSONOutput, Support: runtimecap.HostCapabilitySupported, Source: "fixture"},
		{Capability: runtimecap.HostDurablePolling, Support: runtimecap.HostCapabilityUnknown, Source: "fixture"},
		{Capability: runtimecap.HostResumableFollow, Support: runtimecap.HostCapabilityUnknown, Source: "fixture"},
		{Capability: runtimecap.HostManagedBackgroundWork, Support: runtimecap.HostCapabilityUnknown, Source: "fixture"},
		{Capability: runtimecap.HostCallbacks, Support: runtimecap.HostCapabilityUnknown, Source: "fixture"},
		{Capability: runtimecap.HostWakeUp, Support: runtimecap.HostCapabilityUnknown, Source: "fixture"},
		{Capability: runtimecap.HostAcknowledgment, Support: runtimecap.HostCapabilityUnknown, Source: "fixture"},
		{Capability: runtimecap.HostDetachedSteering, Support: runtimecap.HostCapabilityUnknown, Source: "fixture"},
		{Capability: runtimecap.HostDetachedCancellation, Support: runtimecap.HostCapabilityUnknown, Source: "fixture"},
	}
	for _, capability := range extra {
		for i := range base {
			if base[i].Capability == capability {
				base[i].Support = runtimecap.HostCapabilitySupported
			}
		}
	}
	return base
}

func originDeclaration(opaque string, metadata map[string]string) runtimecap.HostRunOriginDeclaration {
	return runtimecap.HostRunOriginDeclaration{
		SchemaVersion: runtimecap.HostRunOriginSchemaVersion,
		Kind:          "callback-session",
		OpaqueID:      opaque,
		Metadata:      metadata,
	}
}

func stageCode(stages []runtimecap.HostProgressStageRecord, stage runtimecap.HostProgressStage) string {
	for _, record := range stages {
		if record.Stage == stage {
			return record.Code
		}
	}
	return ""
}

func capabilitySupport(capabilities []runtimecap.HostCapabilityDeclaration, capability runtimecap.HostCapability) runtimecap.HostCapabilitySupport {
	for _, declaration := range capabilities {
		if declaration.Capability == capability {
			return declaration.Support
		}
	}
	return runtimecap.HostCapabilityUnknown
}
