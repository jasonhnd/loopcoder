package runtimecap

import (
	"sort"
	"strings"
)

const HostNegotiationSchemaVersion = "loopcoder.host_negotiation.v1"

type HostCapability string

const (
	HostLocalSubprocess HostCapability = "local-subprocess"
	HostStdout          HostCapability = "stdout"
	HostStderr          HostCapability = "stderr"
	HostJSONOutput      HostCapability = "json-output"
	HostTimeouts        HostCapability = "timeouts"
	HostCancellation    HostCapability = "cancellation"
	HostHooks           HostCapability = "hooks"
)

type HostFeature string

const (
	HostFeatureProfile            HostFeature = "host-profile"
	HostFeatureCapabilities       HostFeature = "capability-records"
	HostFeatureInputs             HostFeature = "input-contract"
	HostFeatureOutputs            HostFeature = "output-contract"
	HostFeatureStreaming          HostFeature = "streaming"
	HostFeatureCancellation       HostFeature = "cancellation"
	HostFeatureCompatibility      HostFeature = "compatibility"
	HostFeatureInvocationMetadata HostFeature = "invocation-metadata"
)

type HostCapabilitySupport string

const (
	HostCapabilitySupported   HostCapabilitySupport = "supported"
	HostCapabilityUnsupported HostCapabilitySupport = "unsupported"
	HostCapabilityUnknown     HostCapabilitySupport = "unknown"
)

type HostNegotiationOutcome string

const (
	HostNegotiationSupported    HostNegotiationOutcome = "supported"
	HostNegotiationUnsupported  HostNegotiationOutcome = "unsupported"
	HostNegotiationIncompatible HostNegotiationOutcome = "incompatible"
)

const (
	ErrUnsupportedHostSchemaVersion = "ErrUnsupportedHostSchemaVersion"
	ErrUnsupportedHostFeature       = "ErrUnsupportedHostFeature"
	ErrMissingHostMetadata          = "ErrMissingHostMetadata"
	ErrPartialHostMetadata          = "ErrPartialHostMetadata"
	ErrUnsupportedHostCapability    = "ErrUnsupportedHostCapability"
)

type HostNegotiationRequest struct {
	SchemaVersion           string
	SupportedSchemaVersions []string
	RequestedFeatures       []HostFeature
	Host                    HostProfileRecord
	Capabilities            []HostCapabilityDeclaration
}

type HostProfileRecord struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

type HostCapabilityDeclaration struct {
	Capability HostCapability        `json:"capability"`
	Support    HostCapabilitySupport `json:"support"`
	Required   bool                  `json:"required"`
	Source     string                `json:"source,omitempty"`
}

type HostInputRecord struct {
	SchemaVersions []string      `json:"schema_versions"`
	Features       []HostFeature `json:"features"`
}

type HostOutputRecord struct {
	Format                  string `json:"format"`
	StableJSON              bool   `json:"stable_json"`
	CredentialBlind         bool   `json:"credential_blind"`
	IncludesLocalPaths      bool   `json:"includes_local_paths"`
	IncludesCredentialNames bool   `json:"includes_credential_names"`
}

type HostStreamingRecord struct {
	Stdout HostCapabilitySupport `json:"stdout"`
	Stderr HostCapabilitySupport `json:"stderr"`
}

type HostCancellationRecord struct {
	Timeouts     HostCapabilitySupport `json:"timeouts"`
	Cancellation HostCapabilitySupport `json:"cancellation"`
}

type HostCompatibilityRecord struct {
	Outcome             HostNegotiationOutcome `json:"outcome"`
	Code                string                 `json:"code"`
	SelectedSchema      string                 `json:"selected_schema"`
	UnsupportedFeatures []HostFeature          `json:"unsupported_features,omitempty"`
	MissingCapabilities []HostCapability       `json:"missing_capabilities,omitempty"`
	Reasons             []string               `json:"reasons,omitempty"`
}

type HostInvocationMetadata struct {
	SideEffectClass    string `json:"side_effect_class"`
	CredentialBlind    bool   `json:"credential_blind"`
	ProviderLaunch     bool   `json:"provider_launch"`
	ApprovalMutation   bool   `json:"approval_mutation"`
	DiscoveryOnly      bool   `json:"discovery_only"`
	MachineJSONOnly    bool   `json:"machine_json_only"`
	RedactionPolicy    string `json:"redaction_policy"`
	InvocationMetadata string `json:"invocation_metadata"`
}

type HostNegotiation struct {
	SchemaVersion string                      `json:"schema_version"`
	Profile       HostProfileRecord           `json:"profile"`
	Capabilities  []HostCapabilityDeclaration `json:"capabilities"`
	Inputs        HostInputRecord             `json:"inputs"`
	Outputs       HostOutputRecord            `json:"outputs"`
	Streaming     HostStreamingRecord         `json:"streaming"`
	Cancellation  HostCancellationRecord      `json:"cancellation"`
	Compatibility HostCompatibilityRecord     `json:"compatibility"`
	Invocation    HostInvocationMetadata      `json:"invocation"`
}

func HostCapabilityDeclarations(host HostRuntime) []HostCapabilityDeclaration {
	declarations := []HostCapabilityDeclaration{
		{Capability: HostLocalSubprocess, Support: HostCapabilitySupported, Required: true, Source: "runtime-contract"},
		{Capability: HostStdout, Support: supportFromBool(host.PreservesStdout), Required: true, Source: "runtime-contract"},
		{Capability: HostStderr, Support: supportFromBool(host.PreservesStderr), Required: true, Source: "runtime-contract"},
		{Capability: HostJSONOutput, Support: supportFromBool(host.SupportsJSONOutput), Required: true, Source: "runtime-contract"},
		{Capability: HostTimeouts, Support: supportFromBool(host.SupportsTimeouts), Source: "runtime-contract"},
		{Capability: HostCancellation, Support: supportFromBool(host.SupportsCancel), Source: "runtime-contract"},
		{Capability: HostHooks, Support: supportFromBool(host.SupportsHooks), Source: "runtime-contract"},
	}
	sortHostCapabilityDeclarations(declarations)
	return declarations
}

func NegotiateHost(request HostNegotiationRequest) HostNegotiation {
	selectedSchema, schemaCode, schemaReasons := negotiateHostSchema(request)
	requestedFeatures := sortedHostFeatures(request.RequestedFeatures)
	if len(requestedFeatures) == 0 {
		requestedFeatures = supportedHostFeatures()
	}
	unsupportedFeatures := unsupportedHostFeatures(requestedFeatures)
	capabilities := normalizeHostCapabilities(request.Capabilities)
	missingMetadata := len(capabilities) == 0
	missingRequired, unsupportedRequired := evaluateRequiredHostCapabilities(capabilities)

	outcome := HostNegotiationSupported
	code := "supported"
	reasons := []string{}
	if schemaCode != "" {
		outcome = HostNegotiationIncompatible
		code = schemaCode
		reasons = append(reasons, schemaReasons...)
	} else if len(unsupportedFeatures) > 0 {
		outcome = HostNegotiationUnsupported
		code = ErrUnsupportedHostFeature
		reasons = append(reasons, "requested host feature is not supported by this loopcoder binary")
	} else if missingMetadata {
		outcome = HostNegotiationIncompatible
		code = ErrMissingHostMetadata
		reasons = append(reasons, "host capability metadata is required and was not provided")
	} else if len(missingRequired) > 0 {
		outcome = HostNegotiationUnsupported
		code = ErrPartialHostMetadata
		reasons = append(reasons, "required host capability metadata is missing or unknown")
	} else if len(unsupportedRequired) > 0 {
		outcome = HostNegotiationUnsupported
		code = ErrUnsupportedHostCapability
		reasons = append(reasons, "host does not satisfy one or more required capabilities")
	}

	return HostNegotiation{
		SchemaVersion: HostNegotiationSchemaVersion,
		Profile: HostProfileRecord{
			Name:   stableDiagnosticValue(request.Host.Name, "unspecified"),
			Source: stableDiagnosticValue(request.Host.Source, "unspecified"),
		},
		Capabilities: capabilities,
		Inputs: HostInputRecord{
			SchemaVersions: []string{HostNegotiationSchemaVersion},
			Features:       supportedHostFeatures(),
		},
		Outputs: HostOutputRecord{
			Format:                  "json",
			StableJSON:              true,
			CredentialBlind:         true,
			IncludesLocalPaths:      false,
			IncludesCredentialNames: false,
		},
		Streaming: HostStreamingRecord{
			Stdout: hostCapabilitySupport(capabilities, HostStdout),
			Stderr: hostCapabilitySupport(capabilities, HostStderr),
		},
		Cancellation: HostCancellationRecord{
			Timeouts:     hostCapabilitySupport(capabilities, HostTimeouts),
			Cancellation: hostCapabilitySupport(capabilities, HostCancellation),
		},
		Compatibility: HostCompatibilityRecord{
			Outcome:             outcome,
			Code:                code,
			SelectedSchema:      selectedSchema,
			UnsupportedFeatures: unsupportedFeatures,
			MissingCapabilities: appendHostCapabilities(missingRequired, unsupportedRequired),
			Reasons:             sortedStrings(reasons),
		},
		Invocation: HostInvocationMetadata{
			SideEffectClass:    "none",
			CredentialBlind:    true,
			ProviderLaunch:     false,
			ApprovalMutation:   false,
			DiscoveryOnly:      true,
			MachineJSONOnly:    true,
			RedactionPolicy:    "diagnostic-identifiers-only",
			InvocationMetadata: "provider-neutral",
		},
	}
}

func negotiateHostSchema(request HostNegotiationRequest) (string, string, []string) {
	supported := uniqueStrings(request.SupportedSchemaVersions)
	if len(supported) == 0 && strings.TrimSpace(request.SchemaVersion) != "" {
		supported = []string{strings.TrimSpace(request.SchemaVersion)}
	}
	if len(supported) == 0 {
		return HostNegotiationSchemaVersion, "", nil
	}
	for _, version := range supported {
		if version == HostNegotiationSchemaVersion {
			return HostNegotiationSchemaVersion, "", nil
		}
	}
	return "", ErrUnsupportedHostSchemaVersion, []string{"no shared host negotiation schema version"}
}

func supportedHostFeatures() []HostFeature {
	return []HostFeature{
		HostFeatureCapabilities,
		HostFeatureCancellation,
		HostFeatureCompatibility,
		HostFeatureInputs,
		HostFeatureInvocationMetadata,
		HostFeatureOutputs,
		HostFeatureProfile,
		HostFeatureStreaming,
	}
}

func unsupportedHostFeatures(features []HostFeature) []HostFeature {
	supported := map[HostFeature]bool{}
	for _, feature := range supportedHostFeatures() {
		supported[feature] = true
	}
	var unsupported []HostFeature
	for _, feature := range features {
		if !supported[feature] {
			unsupported = append(unsupported, stableHostFeature(feature))
		}
	}
	return sortedHostFeatures(unsupported)
}

func normalizeHostCapabilities(capabilities []HostCapabilityDeclaration) []HostCapabilityDeclaration {
	byName := map[HostCapability]HostCapabilityDeclaration{}
	for _, capability := range capabilities {
		name := HostCapability(strings.TrimSpace(string(capability.Capability)))
		if name == "" {
			continue
		}
		if !knownHostCapability(name) {
			continue
		}
		support := capability.Support
		switch support {
		case HostCapabilitySupported, HostCapabilityUnsupported, HostCapabilityUnknown:
		default:
			support = HostCapabilityUnknown
		}
		source := stableDiagnosticValue(capability.Source, "host-metadata")
		byName[name] = HostCapabilityDeclaration{
			Capability: name,
			Support:    support,
			Required:   capability.Required || requiredHostCapabilities()[name],
			Source:     source,
		}
	}
	out := make([]HostCapabilityDeclaration, 0, len(byName))
	for _, capability := range byName {
		out = append(out, capability)
	}
	sortHostCapabilityDeclarations(out)
	return out
}

func evaluateRequiredHostCapabilities(capabilities []HostCapabilityDeclaration) ([]HostCapability, []HostCapability) {
	byName := map[HostCapability]HostCapabilitySupport{}
	for _, capability := range capabilities {
		byName[capability.Capability] = capability.Support
	}
	var missing []HostCapability
	var unsupported []HostCapability
	for capability := range requiredHostCapabilities() {
		switch byName[capability] {
		case HostCapabilitySupported:
		case HostCapabilityUnsupported:
			unsupported = append(unsupported, capability)
		default:
			missing = append(missing, capability)
		}
	}
	sortHostCapabilities(missing)
	sortHostCapabilities(unsupported)
	return missing, unsupported
}

func requiredHostCapabilities() map[HostCapability]bool {
	return map[HostCapability]bool{
		HostLocalSubprocess: true,
		HostStdout:          true,
		HostStderr:          true,
		HostJSONOutput:      true,
	}
}

func knownHostCapability(capability HostCapability) bool {
	switch capability {
	case HostLocalSubprocess, HostStdout, HostStderr, HostJSONOutput, HostTimeouts, HostCancellation, HostHooks:
		return true
	default:
		return false
	}
}

func hostCapabilitySupport(capabilities []HostCapabilityDeclaration, target HostCapability) HostCapabilitySupport {
	for _, capability := range capabilities {
		if capability.Capability == target {
			return capability.Support
		}
	}
	return HostCapabilityUnknown
}

func supportFromBool(value bool) HostCapabilitySupport {
	if value {
		return HostCapabilitySupported
	}
	return HostCapabilityUnsupported
}

func sortHostCapabilityDeclarations(capabilities []HostCapabilityDeclaration) {
	sort.Slice(capabilities, func(i, j int) bool {
		return capabilities[i].Capability < capabilities[j].Capability
	})
}

func sortedHostFeatures(features []HostFeature) []HostFeature {
	out := append([]HostFeature(nil), features...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortHostCapabilities(capabilities []HostCapability) {
	sort.Slice(capabilities, func(i, j int) bool { return capabilities[i] < capabilities[j] })
}

func appendHostCapabilities(groups ...[]HostCapability) []HostCapability {
	var out []HostCapability
	for _, group := range groups {
		out = append(out, group...)
	}
	sortHostCapabilities(out)
	return out
}

func stableDiagnosticValue(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, `/\`) || likelySecret(value) {
		return fallback
	}
	return value
}

func stableHostFeature(feature HostFeature) HostFeature {
	value := stableDiagnosticValue(string(feature), "redacted-unsupported-feature")
	return HostFeature(value)
}

func likelySecret(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{"token", "secret", "apikey", "api_key", "password", "bearer", "ghp_", "sk-"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func sortedStrings(values []string) []string {
	out := uniqueStrings(values)
	if len(out) == 0 {
		return nil
	}
	return out
}
