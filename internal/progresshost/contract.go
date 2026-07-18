package progresshost

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/hostprofile"
	"github.com/jasonhnd/loopcoder/internal/progress"
	"github.com/jasonhnd/loopcoder/internal/runtimecap"
)

// HostProgressContract is the truthful, negotiated progress visibility contract
// for one host profile. Active delivery never requires a model call.
type HostProgressContract struct {
	Profile               string                     `json:"profile"`
	DisplayName           string                     `json:"display_name"`
	ForegroundSink        progress.SinkKind          `json:"foreground_sink"`
	DetachedFallback      string                     `json:"detached_fallback"`
	StrictJSONBehavior    string                     `json:"strict_json_behavior"`
	ReconnectReplay       string                     `json:"reconnect_replay"`
	Unsupported           []string                   `json:"unsupported,omitempty"`
	Capabilities          progress.SinkCapabilities  `json:"capabilities"`
	OriginTransport       string                     `json:"origin_transport,omitempty"`
	DetectionCannotGrant  string                     `json:"detection_cannot_grant"`
	Degradation           string                     `json:"degradation"`
}

// BuiltInHostProgressContracts returns the shipped host contracts. Unknown hosts
// always resolve to generic-cli.
func BuiltInHostProgressContracts() []HostProgressContract {
	return []HostProgressContract{
		{
			Profile:          "codex-cli",
			DisplayName:      "Codex CLI",
			ForegroundSink:   progress.SinkKindTerminalHuman,
			DetachedFallback: "durable outbox + status --receipts / attach; optional host-run-origin replay on next Codex turn",
			StrictJSONBehavior: "progress never writes machine command stdout; human lines use stderr; optional JSONL on a dedicated event stream",
			ReconnectReplay:  "undelivered obligations replay via host-run-origin when CODEX_THREAD_ID binds; otherwise next-invocation replay",
			Unsupported:      []string{"private Codex push API", "provider-native nested progress"},
			Capabilities: progress.SinkCapabilities{
				HumanReadable:             true,
				SeparateFromMachineStdout: true,
			},
			OriginTransport:      runtimecap.HostProgressKnownOriginReplay,
			DetectionCannotGrant: "host detection never grants repo write, orchestrate, or native delegation",
			Degradation:          "without a bound thread id, fall back to generic human stderr + outbox-only semantics",
		},
		{
			Profile:          "claude-code",
			DisplayName:      "Claude Code",
			ForegroundSink:   progress.SinkKindTerminalHuman,
			DetachedFallback: "durable outbox + status --receipts / attach; optional host-run-origin replay on next Claude session",
			StrictJSONBehavior: "progress never writes machine command stdout; human lines use stderr",
			ReconnectReplay:  "undelivered obligations replay via host-run-origin when CLAUDE_CODE_SESSION_ID binds",
			Unsupported:      []string{"private Claude Code push API", "provider-native nested progress"},
			Capabilities: progress.SinkCapabilities{
				HumanReadable:             true,
				SeparateFromMachineStdout: true,
			},
			OriginTransport:      runtimecap.HostProgressKnownOriginReplay,
			DetectionCannotGrant: "host detection never grants repo write, orchestrate, or native delegation",
			Degradation:          "without a bound session id, fall back to generic human stderr + outbox-only semantics",
		},
		{
			Profile:          "paseo-style",
			DisplayName:      "Paseo",
			ForegroundSink:   progress.SinkKindTerminalHuman,
			DetachedFallback: "durable outbox + status --receipts / attach; host-run-origin replay when PASEO_AGENT_ID binds",
			StrictJSONBehavior: "progress never writes machine command stdout; human lines use stderr; LoopCoder core does not depend on Paseo UI",
			ReconnectReplay:  "undelivered obligations replay via host-run-origin when agent id binds",
			Unsupported:      []string{"Paseo proprietary UI embedding", "private Paseo-only APIs in core"},
			Capabilities: progress.SinkCapabilities{
				HumanReadable:             true,
				SeparateFromMachineStdout: true,
			},
			OriginTransport:      runtimecap.HostProgressKnownOriginReplay,
			DetectionCannotGrant: "host detection never grants repo write, orchestrate, or native delegation",
			Degradation:          "without PASEO_AGENT_ID, behave as generic-cli",
		},
		{
			Profile:          "generic-cli",
			DisplayName:      "Generic CLI / terminal / automation",
			ForegroundSink:   progress.SinkKindTerminalHuman,
			DetachedFallback: "durable outbox + status --receipts / attach only; no proprietary hooks",
			StrictJSONBehavior: "progress never writes machine command stdout; human lines use stderr; PreferJSONL uses a dedicated event writer when configured",
			ReconnectReplay:  "status/attach and next-invocation outbox replay only",
			Unsupported:      []string{"host-run-origin binding", "host-callback transport"},
			Capabilities: progress.SinkCapabilities{
				HumanReadable:             true,
				JSONL:                     true,
				OutboxOnly:                true,
				SeparateFromMachineStdout: true,
			},
			OriginTransport:      runtimecap.HostProgressNextInvocationReplay,
			DetectionCannotGrant: "unknown hosts never claim richer transports",
			Degradation:          "default for unknown hosts; never claims host-callback",
		},
	}
}

// ContractForProfile returns the built-in contract for a normalized profile name.
func ContractForProfile(profile string) HostProgressContract {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		profile = "generic-cli"
	}
	if name, ok := hostprofile.NormalizeName(profile); ok {
		profile = name
	}
	for _, contract := range BuiltInHostProgressContracts() {
		if contract.Profile == profile {
			return contract
		}
	}
	// Unknown hosts always degrade to generic.
	generic := ContractForProfile("generic-cli")
	generic.Profile = "generic-cli"
	generic.Degradation = fmt.Sprintf("unknown host %q treated as generic-cli", profile)
	return generic
}

// ResolveHostProfile picks the active host profile for progress negotiation.
func ResolveHostProfile(getenv func(string) string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	if raw := strings.TrimSpace(getenv(hostprofile.EnvName)); raw != "" {
		if name, ok := hostprofile.NormalizeName(raw); ok {
			return name
		}
		return "generic-cli"
	}
	for _, candidate := range []struct {
		name string
		vars []string
	}{
		{name: "paseo-style", vars: []string{"PASEO_AGENT_ID"}},
		{name: "claude-code", vars: []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID"}},
		{name: "codex-cli", vars: []string{"CODEX_CLI", "CODEX_THREAD_ID"}},
	} {
		for _, key := range candidate.vars {
			if strings.TrimSpace(getenv(key)) != "" {
				return candidate.name
			}
		}
	}
	return "generic-cli"
}

// ActiveSinkOptions configures host-aware sink negotiation for one invocation.
type ActiveSinkOptions struct {
	HumanWriter io.Writer
	EventWriter io.Writer
	PreferJSONL bool
	ForceOutbox bool
	Getenv      func(string) string
	Now         func() time.Time
	// HostDeliver optionally supplies a true host callback (tests / approved integrations).
	HostDeliver progress.DeliveryFunc
}

// NegotiateActiveSink selects the active foreground sink for the current host
// and returns the negotiation record plus the host contract that applies.
func NegotiateActiveSink(opts ActiveSinkOptions) (progress.Sink, progress.SinkNegotiation, HostProgressContract) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	profile := ResolveHostProfile(getenv)
	contract := ContractForProfile(profile)

	negOpts := progress.NegotiateOptions{
		PreferJSONL:     opts.PreferJSONL,
		HumanWriter:     opts.HumanWriter,
		EventWriter:     opts.EventWriter,
		HostDeliver:     opts.HostDeliver,
		ForceOutboxOnly: opts.ForceOutbox,
		Now:             opts.Now,
	}
	// Only known host profiles may claim host-callback, and only when a real
	// callback function is provided. Detection alone never invents a callback.
	if opts.HostDeliver == nil {
		negOpts.HostDeliver = nil
	} else if profile == "generic-cli" {
		negOpts.HostDeliver = nil
	}

	sink, negotiation := progress.NegotiateSink(negOpts)
	// Annotate reason with host contract identity for operators.
	if negotiation.Reason == "" {
		negotiation.Reason = "host=" + contract.Profile
	} else {
		negotiation.Reason = negotiation.Reason + "; host=" + contract.Profile
	}
	return sink, negotiation, contract
}
