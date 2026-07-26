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
	ProviderBoundedWrite    ProviderCapability = "bounded-write"
	ProviderNestedSubagents ProviderCapability = "nested-subagents"
	ProviderJSONOutput      ProviderCapability = "json-output"
	ProviderMCPConfig       ProviderCapability = "mcp-config"
	ProviderCancellation    ProviderCapability = "cancellation"
	ProviderTokenUsage      ProviderCapability = "token-usage"
)

type CompatibilityRole string

const (
	RoleWorker          CompatibilityRole = "worker"
	RoleVerifier        CompatibilityRole = "verifier"
	RoleNestedSubagents CompatibilityRole = "nested-subagents"
)

type SupportLevel string

const (
	SupportSupported    SupportLevel = "supported"
	SupportExperimental SupportLevel = "experimental"
	SupportUnsupported  SupportLevel = "unsupported"
)

type ProviderRuntime struct {
	Name                     string
	AdapterVersion           string
	DeclarationSchemaVersion string
	DisplayName              string
	Vendor                   string
	Executable               string
	VersionArgv              []string
	ReadOnly                 bool
	BoundedWrite             bool
	NestedSubagents          bool
	JSONOutput               bool
	MCPConfig                bool
	Cancellation             bool
	TokenUsageReporting      bool
	AuthProbeCommand         []string
	AuthProbeParser          string
	AuthArtifactPaths        []string
	AuthEnvironmentNames     []string
	AuthUnsupportedReason    string
	CatalogProbeCommand      []string
	CatalogProbeParser       string
	CatalogProbeMayNetwork   bool
	StaticModelCatalog       []ProviderModelCapability
	MayNetwork               bool
	ConformanceVersion       string
	KnownLimitations         []string
	UnsupportedSuggestions   map[ProviderCapability]string
	// Exact route dimension proof support for capacity-bound product routes.
	// Single authoritative table used by auto-route AND explicit pin.
	// Fixture is never listed — production-ineligible.
	ExactAccountAffirm    bool // auth_binding after launch (auth.json / grokauth)
	ExactInstallAffirm    bool // install_binding from launched executable pinst
	ExactDepthAffirm      bool // stream or accepted_invocation exact effort
	ExactModelAffirm      bool // stream or accepted_invocation exact model
	ExactPermissionAffirm bool // accepted_invocation from exact sandbox/permission options
	// ProductionEligible is false for fixture/test-only adapters.
	ProductionEligible bool
}

type ProviderModelCapability struct {
	ModelID             string
	DisplayName         string
	ReadOnly            bool
	NestedSubagents     bool
	JSONOutput          bool
	MCPConfig           bool
	Cancellation        bool
	TokenUsageReporting bool
	ImageInput          bool
	ImageOutput         bool
	RolesSupported      []CompatibilityRole
	AvailabilityState   string
	LifecycleState      string
	ReplacementModelID  string
	Aliases             []string
	Constraints         []string
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

type CompatibilityEntry struct {
	Provider             string
	Host                 string
	Role                 CompatibilityRole
	Support              SupportLevel
	Code                 string
	RequiredCapabilities []ProviderCapability
	MissingCapabilities  []ProviderCapability
	KnownLimitations     []string
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

func HostNames() []string {
	return DefaultContract().HostNames()
}

func LookupProvider(name string) (ProviderRuntime, bool) {
	return DefaultContract().LookupProvider(name)
}

// ExactRouteAffirmation is the single authoritative exact-dimension proof
// contract for capacity-bound product routes (auto-route and explicit pin).
// Unknown/fixture providers are never production-eligible.
type ExactRouteAffirmation struct {
	Account            bool
	Install            bool
	Depth              bool
	Model              bool
	Permission         bool
	ProductionEligible bool
}

// ExactRouteAffirm returns proof support for provider. Fixture and unknown
// names return ProductionEligible=false (never capacity-bound product winners).
func ExactRouteAffirm(provider string) ExactRouteAffirmation {
	p, ok := LookupProvider(strings.TrimSpace(provider))
	if !ok || !p.ProductionEligible {
		return ExactRouteAffirmation{}
	}
	return ExactRouteAffirmation{
		Account: p.ExactAccountAffirm, Install: p.ExactInstallAffirm,
		Depth: p.ExactDepthAffirm, Model: p.ExactModelAffirm,
		Permission: p.ExactPermissionAffirm, ProductionEligible: p.ProductionEligible,
	}
}

func LookupHost(name string) (HostRuntime, bool) {
	return DefaultContract().LookupHost(name)
}

func RequireProviderCapability(provider string, capability ProviderCapability) error {
	return DefaultContract().RequireProviderCapability(provider, capability)
}

func SmokeMatrix() []CompatibilityEntry {
	return DefaultContract().SmokeMatrix()
}

func EvaluateCompatibility(providerName, hostName string, role CompatibilityRole) CompatibilityEntry {
	return DefaultContract().EvaluateCompatibility(providerName, hostName, role)
}

func (c Contract) ProviderNames() []string {
	names := make([]string, 0, len(c.Providers))
	for _, provider := range c.Providers {
		names = append(names, provider.Name)
	}
	sort.Strings(names)
	return names
}

func (c Contract) HostNames() []string {
	names := make([]string, 0, len(c.Hosts))
	for _, host := range c.Hosts {
		names = append(names, host.Name)
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

func (c Contract) LookupHost(name string) (HostRuntime, bool) {
	for _, host := range c.Hosts {
		if host.Name == name {
			return cloneHost(host), true
		}
	}
	return HostRuntime{}, false
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

func (c Contract) SmokeMatrix() []CompatibilityEntry {
	roles := []CompatibilityRole{RoleWorker, RoleVerifier, RoleNestedSubagents}
	entries := make([]CompatibilityEntry, 0, len(c.Providers)*len(c.Hosts)*len(roles))
	for _, host := range c.Hosts {
		for _, provider := range c.Providers {
			for _, role := range roles {
				entries = append(entries, c.EvaluateCompatibility(provider.Name, host.Name, role))
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Host != entries[j].Host {
			return entries[i].Host < entries[j].Host
		}
		if entries[i].Provider != entries[j].Provider {
			return entries[i].Provider < entries[j].Provider
		}
		return entries[i].Role < entries[j].Role
	})
	return entries
}

func (c Contract) EvaluateCompatibility(providerName, hostName string, role CompatibilityRole) CompatibilityEntry {
	providerName = strings.TrimSpace(providerName)
	hostName = strings.TrimSpace(hostName)
	if hostName == "" {
		hostName = "generic-local"
	}
	provider, providerOK := c.LookupProvider(providerName)
	host, hostOK := c.LookupHost(hostName)
	entry := CompatibilityEntry{
		Provider: providerName,
		Host:     hostName,
		Role:     role,
		Support:  SupportSupported,
		Code:     "supported",
	}
	if entry.Role == "" {
		entry.Role = RoleWorker
	}
	if !providerOK {
		entry.Support = SupportUnsupported
		entry.Code = "unknown_provider"
		entry.KnownLimitations = []string{"provider is not in the runtime capability contract"}
		return entry
	}
	if !hostOK {
		entry.Support = SupportUnsupported
		entry.Code = "unknown_host"
		entry.KnownLimitations = []string{"host profile is not in the runtime capability contract"}
		return entry
	}
	entry.Provider = provider.Name
	entry.Host = host.Name
	entry.KnownLimitations = append(entry.KnownLimitations, provider.KnownLimitations...)
	entry.KnownLimitations = append(entry.KnownLimitations, host.KnownLimitations...)

	entry.RequiredCapabilities = requiredProviderCapabilities(entry.Role)
	for _, capability := range entry.RequiredCapabilities {
		if !provider.Supports(capability) {
			entry.MissingCapabilities = append(entry.MissingCapabilities, capability)
		}
	}
	if !host.PreservesStdout {
		entry.KnownLimitations = append(entry.KnownLimitations, "host does not preserve stdout")
	}
	if !host.PreservesStderr {
		entry.KnownLimitations = append(entry.KnownLimitations, "host does not preserve stderr")
	}
	if entry.Role == RoleVerifier && !host.SupportsJSONOutput {
		entry.KnownLimitations = append(entry.KnownLimitations, "host does not preserve JSON output")
	}

	if len(entry.MissingCapabilities) > 0 || !host.PreservesStdout || !host.PreservesStderr || (entry.Role == RoleVerifier && !host.SupportsJSONOutput) {
		entry.Support = SupportUnsupported
		entry.Code = compatibilityUnsupportedCode(entry.Role, entry.MissingCapabilities)
		return entry
	}
	if providerExperimental(provider.Name) || hostExperimental(host.Name) {
		entry.Support = SupportExperimental
		entry.Code = "experimental"
		return entry
	}
	return entry
}

func requiredProviderCapabilities(role CompatibilityRole) []ProviderCapability {
	switch role {
	case RoleVerifier:
		return []ProviderCapability{ProviderReadOnly, ProviderJSONOutput, ProviderCancellation}
	case RoleNestedSubagents:
		return []ProviderCapability{ProviderNestedSubagents, ProviderCancellation}
	default:
		return []ProviderCapability{ProviderCancellation}
	}
}

func compatibilityUnsupportedCode(role CompatibilityRole, missing []ProviderCapability) string {
	missingSet := map[ProviderCapability]bool{}
	for _, capability := range missing {
		missingSet[capability] = true
	}
	switch {
	case role == RoleVerifier && missingSet[ProviderReadOnly]:
		return "unsupported_read_only_mode"
	case role == RoleNestedSubagents && missingSet[ProviderNestedSubagents]:
		return "unsupported_nested_agents"
	case missingSet[ProviderJSONOutput]:
		return "unsupported_json_output"
	case missingSet[ProviderMCPConfig]:
		return "unsupported_mcp_config"
	case missingSet[ProviderCancellation]:
		return "unsupported_cancellation"
	default:
		return "unsupported_provider_mode"
	}
}

func providerExperimental(name string) bool {
	switch name {
	case "gemini", "antigravity":
		return true
	default:
		return false
	}
}

func hostExperimental(name string) bool {
	return name == "generic-local"
}

func (p ProviderRuntime) Supports(capability ProviderCapability) bool {
	switch capability {
	case ProviderReadOnly:
		return p.ReadOnly
	case ProviderBoundedWrite:
		return p.BoundedWrite
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
		if unsafeIdentifier(name) {
			violations = append(violations, fmt.Sprintf("provider %q name contains a path separator", name))
		}
		if seenProviders[name] {
			violations = append(violations, fmt.Sprintf("provider %q is duplicated", name))
		}
		seenProviders[name] = true
		if strings.TrimSpace(provider.Executable) == "" {
			violations = append(violations, fmt.Sprintf("provider %q executable is empty", name))
		}
		if unsafeIdentifier(provider.Executable) {
			violations = append(violations, fmt.Sprintf("provider %q executable %q must be a command name, not a path", name, provider.Executable))
		}
		if len(provider.VersionArgv) > 0 {
			violations = append(violations, fixedArgvViolations(name, "version argv", provider.VersionArgv, false)...)
		}
		if len(provider.AuthProbeCommand) > 0 && strings.TrimSpace(provider.AuthProbeCommand[0]) == "" {
			violations = append(violations, fmt.Sprintf("provider %q auth probe executable is empty", name))
		}
		if len(provider.AuthProbeCommand) > 0 {
			violations = append(violations, fixedArgvViolations(name, "auth probe command", provider.AuthProbeCommand, true)...)
		}
		if len(provider.CatalogProbeCommand) > 0 {
			violations = append(violations, fixedArgvViolations(name, "catalog probe command", provider.CatalogProbeCommand, true)...)
		}
		if provider.MayNetwork && len(provider.AuthProbeCommand) == 0 {
			violations = append(violations, fmt.Sprintf("provider %q declares network auth probing without an auth probe command", name))
		}
		if provider.CatalogProbeMayNetwork && len(provider.CatalogProbeCommand) == 0 {
			violations = append(violations, fmt.Sprintf("provider %q declares network catalog probing without a catalog probe command", name))
		}
		if len(provider.AuthProbeCommand) == 0 && len(provider.AuthArtifactPaths) == 0 && len(provider.AuthEnvironmentNames) == 0 && strings.TrimSpace(provider.AuthUnsupportedReason) == "" {
			violations = append(violations, fmt.Sprintf("provider %q must declare credential-blind auth evidence or an unsupported reason", name))
		}
		for _, envName := range provider.AuthEnvironmentNames {
			if strings.TrimSpace(envName) == "" || strings.Contains(envName, "=") {
				violations = append(violations, fmt.Sprintf("provider %q auth environment name %q is invalid", name, envName))
			}
		}
		for _, model := range provider.StaticModelCatalog {
			if strings.TrimSpace(model.ModelID) == "" {
				violations = append(violations, fmt.Sprintf("provider %q static catalog model id is empty", name))
			}
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

func unsafeIdentifier(value string) bool {
	return strings.ContainsAny(strings.TrimSpace(value), `/\`)
}

func fixedArgvViolations(providerName, field string, argv []string, commandIncludesExecutable bool) []string {
	var violations []string
	for index, arg := range argv {
		if strings.TrimSpace(arg) == "" {
			violations = append(violations, fmt.Sprintf("provider %q %s[%d] is empty", providerName, field, index))
		}
	}
	if commandIncludesExecutable && len(argv) > 0 && unsafeIdentifier(argv[0]) {
		violations = append(violations, fmt.Sprintf("provider %q %s executable %q must be a command name, not a path", providerName, field, argv[0]))
	}
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
	hosts := make([]HostRuntime, 0, len(contract.Hosts))
	for _, host := range contract.Hosts {
		hosts = append(hosts, cloneHost(host))
	}
	return Contract{Providers: providers, Hosts: hosts}
}

func cloneProvider(provider ProviderRuntime) ProviderRuntime {
	provider.VersionArgv = append([]string(nil), provider.VersionArgv...)
	provider.AuthProbeCommand = append([]string(nil), provider.AuthProbeCommand...)
	provider.AuthArtifactPaths = append([]string(nil), provider.AuthArtifactPaths...)
	provider.AuthEnvironmentNames = append([]string(nil), provider.AuthEnvironmentNames...)
	provider.CatalogProbeCommand = append([]string(nil), provider.CatalogProbeCommand...)
	provider.StaticModelCatalog = cloneStaticModelCatalog(provider.StaticModelCatalog)
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

func cloneStaticModelCatalog(models []ProviderModelCapability) []ProviderModelCapability {
	out := make([]ProviderModelCapability, 0, len(models))
	for _, model := range models {
		model.RolesSupported = append([]CompatibilityRole(nil), model.RolesSupported...)
		model.Aliases = append([]string(nil), model.Aliases...)
		model.Constraints = append([]string(nil), model.Constraints...)
		out = append(out, model)
	}
	return out
}

func cloneHost(host HostRuntime) HostRuntime {
	host.KnownLimitations = append([]string(nil), host.KnownLimitations...)
	return host
}

var staticContract = Contract{
	Providers: []ProviderRuntime{
		{
			Name:                "codex",
			AdapterVersion:      "v1",
			DisplayName:         "Codex",
			Vendor:              "OpenAI Codex",
			Executable:          "codex",
			VersionArgv:         []string{"--version"},
			ReadOnly:            true,
			BoundedWrite:        true,
			JSONOutput:          true,
			MCPConfig:           true,
			Cancellation:        true,
			TokenUsageReporting: true,
			ExactAccountAffirm:  true, ExactInstallAffirm: true, ExactDepthAffirm: true,
			ExactModelAffirm: true, ExactPermissionAffirm: true, ProductionEligible: true,
			// `codex login status` is the provider-sanctioned local status
			// command; it reports login state without requiring LoopCoder to
			// read Codex credential files or token-bearing config.
			AuthProbeCommand: []string{
				"codex",
				"login",
				"status",
			},
			AuthProbeParser: "codex-login-status",
			MayNetwork:      false,
		},
		{
			Name:                "claude",
			AdapterVersion:      "v1",
			DisplayName:         "Claude",
			Vendor:              "Anthropic",
			Executable:          "claude",
			VersionArgv:         []string{"--version"},
			ReadOnly:            true,
			NestedSubagents:     true,
			JSONOutput:          true,
			MCPConfig:           true,
			Cancellation:        true,
			TokenUsageReporting: true,
			// Exact account binding is re-observed from the same executable's
			// machine-readable auth status immediately before invocation.
			ExactAccountAffirm: true, ExactInstallAffirm: true, ExactDepthAffirm: true,
			ExactModelAffirm: true, ExactPermissionAffirm: true, ProductionEligible: true,
			// `claude auth status --json` is a local machine-readable status
			// surface with declared non-secret fields; LoopCoder persists only
			// redacted displays, hashes, and summarized authorization metadata.
			AuthProbeCommand: []string{
				"claude",
				"auth",
				"status",
				"--json",
			},
			AuthProbeParser: "claude-auth-status-json",
			MayNetwork:      false,
		},
		{
			Name:                "gemini",
			AdapterVersion:      "v1",
			DisplayName:         "Gemini",
			Vendor:              "Google",
			Executable:          "gemini",
			VersionArgv:         []string{"--version"},
			ReadOnly:            true,
			JSONOutput:          true,
			MCPConfig:           true,
			Cancellation:        true,
			TokenUsageReporting: true,
			// No account; no depth knob.
			ExactAccountAffirm: false, ExactInstallAffirm: true, ExactDepthAffirm: false,
			ExactModelAffirm: true, ExactPermissionAffirm: true, ProductionEligible: true,
			// Gemini currently has no sanctioned credential-blind local status
			// command here, so LoopCoder checks only whether declared auth
			// references exist and never reads their values or file contents.
			AuthArtifactPaths: []string{
				"~/.gemini/oauth_creds.json",
			},
			AuthEnvironmentNames: []string{
				"GEMINI_API_KEY",
				"GOOGLE_API_KEY",
			},
			AuthUnsupportedReason: "gemini adapter has no dedicated credential-blind auth status command; only secret-reference existence can be reported",
			MayNetwork:            false,
			KnownLimitations: []string{
				"experimental and not part of the static model registry",
				"ignores reasoning effort because the CLI has no separate effort knob",
			},
		},
		{
			Name:           "antigravity",
			AdapterVersion: "v1",
			DisplayName:    "Antigravity",
			Vendor:         "Google Antigravity",
			Executable:     "agy",
			VersionArgv:    []string{"--version"},
			Cancellation:   true,
			// Runner rejects exact AccountRef.
			ExactAccountAffirm: false, ExactInstallAffirm: true, ExactDepthAffirm: true,
			ExactModelAffirm: true, ExactPermissionAffirm: true, ProductionEligible: true,
			// `agy models` is the narrow provider surface available for
			// auth/model reachability, but it may contact the network; runtime
			// policy therefore records it and skips it by default.
			AuthProbeCommand: []string{
				"agy",
				"models",
			},
			AuthProbeParser: "agy-models",
			MayNetwork:      true,
			CatalogProbeCommand: []string{
				"agy",
				"models",
			},
			CatalogProbeParser:     "agy-models",
			CatalogProbeMayNetwork: true,
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
		{
			Name:                "grok",
			AdapterVersion:      "v1",
			DisplayName:         "Grok Build",
			Vendor:              "xAI",
			Executable:          "grok",
			VersionArgv:         []string{"version"},
			ReadOnly:            true,
			BoundedWrite:        true,
			JSONOutput:          true,
			Cancellation:        true,
			TokenUsageReporting: true,
			// grokauth + pinst + stream/accepted_invocation for model/effort/permission.
			ExactAccountAffirm: true, ExactInstallAffirm: true, ExactDepthAffirm: true,
			ExactModelAffirm: true, ExactPermissionAffirm: true, ProductionEligible: true,
			// `grok models` is the documented read-only inventory surface for
			// Grok Build. It may require account/network reachability, but it
			// does not launch an agent session, open login, install updates, or
			// load marketplace plugins.
			AuthProbeCommand: []string{
				"grok",
				"models",
			},
			AuthProbeParser: "grok-models",
			MayNetwork:      true,
			CatalogProbeCommand: []string{
				"grok",
				"models",
			},
			CatalogProbeParser:     "grok-models",
			CatalogProbeMayNetwork: true,
			AuthEnvironmentNames: []string{
				"XAI_API_KEY",
			},
			KnownLimitations: []string{
				"bounded execution requires installed CLI help to advertise the required headless flags",
				"project-local Claude/Grok settings, hooks, plugins, agents, MCP, or memory files cause fail-closed output because no documented ignore-project-config flag is available",
				"user home/config/temp roots are replaced with private per-attempt directories; XAI_API_KEY is the only provider credential environment variable passed through",
				"schema-enforced verifier JSON, MCP configuration injection, native subagents, quota collection, and cross-provider handoff are not implemented",
				"token usage and cost are recorded only when the CLI streaming output supplies them",
			},
			UnsupportedSuggestions: map[ProviderCapability]string{
				ProviderMCPConfig: "do not select Grok Build for MCP-injected loopcoder worker or verifier dispatch",
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
				"LoopCoder has no documented Paseo callback, targeted wake, acknowledgment, poll, or follow proof in this release; progress delivery is LoopCoder-local status/attach plus matching-origin next-invocation replay",
				"upstream Paseo issue #2034 can delay an already-closed Claude ProcessTransport refresh by the rescue timeout; LoopCoder treats this as an isolated host limitation",
			},
		},
		{
			Name:               "generic-local",
			InvocationStyle:    "unknown local agent host calls loopcoder as a subprocess",
			PreservesStdout:    true,
			PreservesStderr:    true,
			SupportsJSONOutput: true,
			SupportsTimeouts:   true,
			SupportsCancel:     true,
			KnownLimitations: []string{
				"host-specific hook behavior and stderr relay guarantees are not known; use LOOPCODER_HOST or host.profile when invoking from a known host",
			},
		},
	},
}

func init() {
	staticContract.MustBeValid()
}
