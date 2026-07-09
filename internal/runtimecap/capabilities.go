// Package runtimecap defines loopcoder's provider and host runtime capability contract.
package runtimecap

import (
	"fmt"
	"sort"
	"strings"
)

type ProviderCapability string

const (
	ProviderReadOnly        ProviderCapability = "read-only"
	ProviderNestedSubagents ProviderCapability = "nested-subagents"
	ProviderJSONOutput      ProviderCapability = "json-output"
	ProviderMCPConfig       ProviderCapability = "mcp-config"
	ProviderCancellation    ProviderCapability = "cancellation"
	ProviderTokenUsage      ProviderCapability = "token-usage"
)

type ProviderRuntime struct {
	Name                   string
	Executable             string
	ReadOnly               bool
	NestedSubagents        bool
	JSONOutput             bool
	MCPConfig              bool
	Cancellation           bool
	TokenUsageReporting    bool
	AuthProbeCommand       []string
	KnownLimitations       []string
	UnsupportedSuggestions map[ProviderCapability]string
}

type HostRuntime struct {
	Name               string
	InvocationStyle    string
	PreservesStdout    bool
	PreservesStderr    bool
	SupportsJSONOutput bool
	SupportsTimeouts   bool
	SupportsCancel     bool
	SupportsHooks      bool
	KnownLimitations   []string
}

type Contract struct {
	Providers []ProviderRuntime
	Hosts     []HostRuntime
}

type UnsupportedCapabilityError struct {
	Provider    string
	Capability  ProviderCapability
	Suggestion  string
	Supported   []string
	Limitations []string
}

func (e UnsupportedCapabilityError) Error() string {
	parts := []string{fmt.Sprintf("provider %q does not support %s", e.Provider, e.Capability)}
	if len(e.Supported) > 0 {
		parts = append(parts, "choose a supporting provider: "+strings.Join(e.Supported, ", "))
	}
	if strings.TrimSpace(e.Suggestion) != "" {
		parts = append(parts, strings.TrimSpace(e.Suggestion))
	}
	if len(e.Limitations) > 0 {
		parts = append(parts, "known limitation: "+strings.Join(e.Limitations, "; "))
	}
	return strings.Join(parts, "; ")
}

func DefaultContract() Contract {
	return cloneContract(staticContract)
}

func ProviderNames() []string {
	return DefaultContract().ProviderNames()
}

func LookupProvider(name string) (ProviderRuntime, bool) {
	return DefaultContract().LookupProvider(name)
}

func RequireProviderCapability(provider string, capability ProviderCapability) error {
	return DefaultContract().RequireProviderCapability(provider, capability)
}

func (c Contract) ProviderNames() []string {
	names := make([]string, 0, len(c.Providers))
	for _, provider := range c.Providers {
		names = append(names, provider.Name)
	}
	sort.Strings(names)
	return names
}

func (c Contract) LookupProvider(name string) (ProviderRuntime, bool) {
	for _, provider := range c.Providers {
		if provider.Name == name {
			return cloneProvider(provider), true
		}
	}
	return ProviderRuntime{}, false
}

func (c Contract) RequireProviderCapability(providerName string, capability ProviderCapability) error {
	provider, ok := c.LookupProvider(providerName)
	if !ok {
		return fmt.Errorf("unknown provider %q (known providers: %s)", providerName, strings.Join(c.ProviderNames(), ", "))
	}
	if provider.Supports(capability) {
		return nil
	}
	supported := make([]string, 0, len(c.Providers))
	for _, candidate := range c.Providers {
		if candidate.Supports(capability) {
			supported = append(supported, candidate.Name)
		}
	}
	sort.Strings(supported)
	return UnsupportedCapabilityError{
		Provider:    provider.Name,
		Capability:  capability,
		Suggestion:  provider.UnsupportedSuggestions[capability],
		Supported:   supported,
		Limitations: append([]string(nil), provider.KnownLimitations...),
	}
}

func (p ProviderRuntime) Supports(capability ProviderCapability) bool {
	switch capability {
	case ProviderReadOnly:
		return p.ReadOnly
	case ProviderNestedSubagents:
		return p.NestedSubagents
	case ProviderJSONOutput:
		return p.JSONOutput
	case ProviderMCPConfig:
		return p.MCPConfig
	case ProviderCancellation:
		return p.Cancellation
	case ProviderTokenUsage:
		return p.TokenUsageReporting
	default:
		return false
	}
}

func (c Contract) InvariantViolations() []string {
	var violations []string
	seenProviders := map[string]bool{}
	for _, provider := range c.Providers {
		name := strings.TrimSpace(provider.Name)
		if name == "" {
			violations = append(violations, "provider name is empty")
		}
		if seenProviders[name] {
			violations = append(violations, fmt.Sprintf("provider %q is duplicated", name))
		}
		seenProviders[name] = true
		if strings.TrimSpace(provider.Executable) == "" {
			violations = append(violations, fmt.Sprintf("provider %q executable is empty", name))
		}
		if len(provider.AuthProbeCommand) > 0 && strings.TrimSpace(provider.AuthProbeCommand[0]) == "" {
			violations = append(violations, fmt.Sprintf("provider %q auth probe executable is empty", name))
		}
	}

	seenHosts := map[string]bool{}
	for _, host := range c.Hosts {
		name := strings.TrimSpace(host.Name)
		if name == "" {
			violations = append(violations, "host name is empty")
		}
		if seenHosts[name] {
			violations = append(violations, fmt.Sprintf("host %q is duplicated", name))
		}
		seenHosts[name] = true
		if strings.TrimSpace(host.InvocationStyle) == "" {
			violations = append(violations, fmt.Sprintf("host %q invocation style is empty", name))
		}
		if !host.PreservesStdout {
			violations = append(violations, fmt.Sprintf("host %q must preserve stdout", name))
		}
		if !host.PreservesStderr {
			violations = append(violations, fmt.Sprintf("host %q must preserve stderr", name))
		}
	}
	sort.Strings(violations)
	return violations
}

func (c Contract) MustBeValid() {
	if violations := c.InvariantViolations(); len(violations) > 0 {
		panic("invalid runtime capability contract: " + strings.Join(violations, "; "))
	}
}

func cloneContract(contract Contract) Contract {
	providers := make([]ProviderRuntime, 0, len(contract.Providers))
	for _, provider := range contract.Providers {
		providers = append(providers, cloneProvider(provider))
	}
	hosts := append([]HostRuntime(nil), contract.Hosts...)
	for index := range hosts {
		hosts[index].KnownLimitations = append([]string(nil), hosts[index].KnownLimitations...)
	}
	return Contract{Providers: providers, Hosts: hosts}
}

func cloneProvider(provider ProviderRuntime) ProviderRuntime {
	provider.AuthProbeCommand = append([]string(nil), provider.AuthProbeCommand...)
	provider.KnownLimitations = append([]string(nil), provider.KnownLimitations...)
	if provider.UnsupportedSuggestions != nil {
		suggestions := make(map[ProviderCapability]string, len(provider.UnsupportedSuggestions))
		for capability, suggestion := range provider.UnsupportedSuggestions {
			suggestions[capability] = suggestion
		}
		provider.UnsupportedSuggestions = suggestions
	}
	return provider
}

var staticContract = Contract{
	Providers: []ProviderRuntime{
		{
			Name:                "codex",
			Executable:          "codex",
			ReadOnly:            true,
			JSONOutput:          true,
			MCPConfig:           true,
			Cancellation:        true,
			TokenUsageReporting: true,
		},
		{
			Name:                "claude",
			Executable:          "claude",
			ReadOnly:            true,
			NestedSubagents:     true,
			JSONOutput:          true,
			MCPConfig:           true,
			Cancellation:        true,
			TokenUsageReporting: true,
		},
		{
			Name:                "gemini",
			Executable:          "gemini",
			ReadOnly:            true,
			JSONOutput:          true,
			MCPConfig:           true,
			Cancellation:        true,
			TokenUsageReporting: true,
			KnownLimitations: []string{
				"experimental and not part of the static model registry",
				"ignores reasoning effort because the CLI has no separate effort knob",
			},
		},
		{
			Name:         "antigravity",
			Executable:   "agy",
			Cancellation: true,
			AuthProbeCommand: []string{
				"agy",
				"models",
			},
			KnownLimitations: []string{
				"read-only mode is not available or verified",
				"MCP configuration injection is not implemented",
				"stable parseable token usage is not exposed in this path",
				"provider output is captured as plain text rather than schema-enforced JSON",
			},
			UnsupportedSuggestions: map[ProviderCapability]string{
				ProviderReadOnly:   "use antigravity only for write-mode worker dispatch, or select codex/claude for verifier and audit review",
				ProviderMCPConfig:  "remove MCP servers for this invocation, or select a provider with MCP configuration support",
				ProviderJSONOutput: "do not select antigravity for schema-enforced JSON verifier output",
				ProviderTokenUsage: "treat antigravity token usage as not reported",
			},
		},
	},
	Hosts: []HostRuntime{
		{
			Name:               "codex-cli",
			InvocationStyle:    "interactive Codex CLI conductor session calls loopcoder as a local subprocess",
			PreservesStdout:    true,
			PreservesStderr:    true,
			SupportsJSONOutput: true,
			SupportsTimeouts:   true,
			SupportsCancel:     true,
			KnownLimitations: []string{
				"project hook enforcement is best-effort unless manually wired by the host",
			},
		},
		{
			Name:               "claude-code",
			InvocationStyle:    "Claude Code skill or conductor session calls loopcoder as a local subprocess",
			PreservesStdout:    true,
			PreservesStderr:    true,
			SupportsJSONOutput: true,
			SupportsTimeouts:   true,
			SupportsCancel:     true,
			SupportsHooks:      true,
		},
		{
			Name:               "paseo-style",
			InvocationStyle:    "external conductor or agent supervisor calls loopcoder as a local subprocess",
			PreservesStdout:    true,
			PreservesStderr:    true,
			SupportsJSONOutput: true,
			SupportsTimeouts:   true,
			SupportsCancel:     true,
			KnownLimitations: []string{
				"the host owns session lifetime and must keep stderr visible for local relay obligations",
			},
		},
	},
}

func init() {
	staticContract.MustBeValid()
}
