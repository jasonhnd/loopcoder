package runtimecap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

const HostNegotiationSchemaVersion = "loopcoder.host_negotiation.v1"
const HostRunOriginSchemaVersion = "loopcoder.host_run_origin.v1"

const (
	maxHostOriginMetadataKeys  = 16
	maxHostOriginMetadataRunes = 2048
	maxHostOriginValueRunes    = 120
)

type HostCapability string

const (
	HostLocalSubprocess       HostCapability = "local-subprocess"
	HostStdout                HostCapability = "stdout"
	HostStderr                HostCapability = "stderr"
	HostJSONOutput            HostCapability = "json-output"
	HostTimeouts              HostCapability = "timeouts"
	HostCancellation          HostCapability = "cancellation"
	HostHooks                 HostCapability = "hooks"
	HostDurablePolling        HostCapability = "durable-polling"
	HostResumableFollow       HostCapability = "resumable-follow"
	HostManagedBackgroundWork HostCapability = "host-managed-background-work"
	HostCallbacks             HostCapability = "callbacks"
	HostWakeUp                HostCapability = "wake-up"
	HostAcknowledgment        HostCapability = "acknowledgment"
	HostDetachedSteering      HostCapability = "detached-steering"
	HostDetachedCancellation  HostCapability = "detached-cancellation"
	HostPayloadLimits         HostCapability = "payload-limits"
	HostRateLimits            HostCapability = "rate-limits"
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
	HostFeatureProgressTransport  HostFeature = "progress-transport"
	HostFeatureRunOrigin          HostFeature = "run-origin"
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
	ErrUnsupportedHostSchemaVersion       = "ErrUnsupportedHostSchemaVersion"
	ErrUnsupportedHostFeature             = "ErrUnsupportedHostFeature"
	ErrMissingHostMetadata                = "ErrMissingHostMetadata"
	ErrPartialHostMetadata                = "ErrPartialHostMetadata"
	ErrUnsupportedHostCapability          = "ErrUnsupportedHostCapability"
	ErrUnsupportedHostOriginSchemaVersion = "ErrUnsupportedHostOriginSchemaVersion"
	ErrInvalidHostOriginScope             = "ErrInvalidHostOriginScope"
	ErrHostOriginMetadataTooLarge         = "ErrHostOriginMetadataTooLarge"
	ErrInvalidHostOriginMetadata          = "ErrInvalidHostOriginMetadata"
)

const (
	HostProgressAcknowledgedStreaming   = "acknowledged-streaming"
	HostProgressUnacknowledgedStreaming = "unacknowledged-streaming"
	HostProgressDurableFollowPoll       = "durable-follow-poll"
	HostProgressKnownOriginReplay       = "known-origin-next-invocation-replay"
	HostProgressNextInvocationReplay    = "next-invocation-replay"

	HostProgressAckRequired = "required-ack"
	HostProgressAckNone     = "no-ack"
)

type HostProgressStage string

const (
	HostProgressStageReceiptGeneration HostProgressStage = "receipt-generation"
	HostProgressStageTransportWrite    HostProgressStage = "transport-write"
	HostProgressStageHostAcceptance    HostProgressStage = "host-acceptance"
	HostProgressStageUserVisibility    HostProgressStage = "user-visibility"
	HostProgressStageAcknowledgment    HostProgressStage = "acknowledgment"
	HostProgressStageWakeUp            HostProgressStage = "wake-up"
)

const (
	HostStageLocalOnly        = "local-only"
	HostStageEvidenceRequired = "evidence-required"
	HostStageUnsupported      = "unsupported"
	HostStageReplayOnly       = "replay-only"
)

const (
	HostOriginAbsent = "origin-absent"
	HostOriginBound  = "origin-bound"
)

type HostNegotiationRequest struct {
	SchemaVersion           string
	SupportedSchemaVersions []string
	RequestedFeatures       []HostFeature
	Host                    HostProfileRecord
	Capabilities            []HostCapabilityDeclaration
	ProgressLimits          HostProgressLimitDeclaration
	Origin                  HostRunOriginBindingRequest
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

type HostProgressLimitDeclaration struct {
	MaxPayloadBytes      int `json:"max_payload_bytes,omitempty"`
	MaxEnvelopeBytes     int `json:"max_envelope_bytes,omitempty"`
	MaxReceiptsPerMinute int `json:"max_receipts_per_minute,omitempty"`
	MaxOutstanding       int `json:"max_outstanding,omitempty"`
}

type HostProgressTransportRecord struct {
	TransportContract string                    `json:"transport_contract"`
	AckPolicy         string                    `json:"ack_policy"`
	FallbackOrder     []string                  `json:"fallback_order"`
	Limits            HostProgressLimitRecord   `json:"limits"`
	Stages            []HostProgressStageRecord `json:"stages"`
}

type HostProgressLimitRecord struct {
	MaxPayloadBytes      int  `json:"max_payload_bytes"`
	MaxEnvelopeBytes     int  `json:"max_envelope_bytes"`
	MaxReceiptsPerMinute int  `json:"max_receipts_per_minute"`
	MaxOutstanding       int  `json:"max_outstanding"`
	Declared             bool `json:"declared"`
}

type HostProgressStageRecord struct {
	Stage            HostProgressStage `json:"stage"`
	Code             string            `json:"code"`
	EvidenceRequired bool              `json:"evidence_required"`
	EvidenceKind     string            `json:"evidence_kind"`
}

type HostRunOriginDeclaration struct {
	SchemaVersion           string            `json:"schema_version,omitempty"`
	SupportedSchemaVersions []string          `json:"supported_schema_versions,omitempty"`
	Kind                    string            `json:"kind,omitempty"`
	OpaqueID                string            `json:"opaque_id,omitempty"`
	Metadata                map[string]string `json:"metadata,omitempty"`
}

type HostRunOriginBindingRequest struct {
	ProjectID     string                   `json:"project_id,omitempty"`
	DeliveryRunID string                   `json:"delivery_run_id,omitempty"`
	CorrelationID string                   `json:"correlation_id,omitempty"`
	Origin        HostRunOriginDeclaration `json:"origin,omitempty"`
}

type HostRunOriginBinding struct {
	Bound          bool           `json:"bound"`
	Code           string         `json:"code"`
	SchemaVersion  string         `json:"schema_version,omitempty"`
	BindingID      string         `json:"binding_id,omitempty"`
	OriginKind     string         `json:"origin_kind,omitempty"`
	OriginRef      string         `json:"origin_ref,omitempty"`
	ProjectID      string         `json:"project_id,omitempty"`
	DeliveryRunID  string         `json:"delivery_run_id,omitempty"`
	CorrelationID  string         `json:"correlation_id,omitempty"`
	MetadataDigest string         `json:"metadata_digest,omitempty"`
	MetadataKeys   []string       `json:"metadata_keys,omitempty"`
	Redacted       bool           `json:"redacted"`
	Limits         map[string]int `json:"limits,omitempty"`
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
	Progress      HostProgressTransportRecord `json:"progress"`
	Origin        HostRunOriginBinding        `json:"origin"`
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
		{Capability: HostDurablePolling, Support: HostCapabilityUnknown, Source: "runtime-contract"},
		{Capability: HostResumableFollow, Support: HostCapabilityUnknown, Source: "runtime-contract"},
		{Capability: HostManagedBackgroundWork, Support: HostCapabilityUnknown, Source: "runtime-contract"},
		{Capability: HostCallbacks, Support: HostCapabilityUnknown, Source: "runtime-contract"},
		{Capability: HostWakeUp, Support: HostCapabilityUnknown, Source: "runtime-contract"},
		{Capability: HostAcknowledgment, Support: HostCapabilityUnknown, Source: "runtime-contract"},
		{Capability: HostDetachedSteering, Support: HostCapabilityUnknown, Source: "runtime-contract"},
		{Capability: HostDetachedCancellation, Support: HostCapabilityUnknown, Source: "runtime-contract"},
		{Capability: HostPayloadLimits, Support: HostCapabilityUnknown, Source: "runtime-contract"},
		{Capability: HostRateLimits, Support: HostCapabilityUnknown, Source: "runtime-contract"},
	}
	if host.Name == "codex-cli" {
		for i := range declarations {
			switch declarations[i].Capability {
			case HostDurablePolling, HostResumableFollow, HostDetachedCancellation:
				declarations[i].Support = HostCapabilitySupported
				declarations[i].Source = "codex-cli-documented-local-surface"
			case HostManagedBackgroundWork, HostCallbacks, HostWakeUp, HostAcknowledgment:
				declarations[i].Support = HostCapabilityUnsupported
				declarations[i].Source = "codex-cli-documented-local-surface"
			}
		}
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
	origin := BindHostRunOrigin(request.Origin)

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
		Progress: negotiateProgressTransport(capabilities, request.ProgressLimits, origin),
		Origin:   origin,
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
		HostFeatureProgressTransport,
		HostFeatureRunOrigin,
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

func negotiateProgressTransport(capabilities []HostCapabilityDeclaration, limits HostProgressLimitDeclaration, origin HostRunOriginBinding) HostProgressTransportRecord {
	ack := capabilityIsSupported(capabilities, HostAcknowledgment)
	callbacks := capabilityIsSupported(capabilities, HostCallbacks)
	wake := capabilityIsSupported(capabilities, HostWakeUp)
	durable := capabilityIsSupported(capabilities, HostDurablePolling)
	follow := capabilityIsSupported(capabilities, HostResumableFollow)

	contract := HostProgressNextInvocationReplay
	ackPolicy := HostProgressAckNone
	switch {
	case callbacks && wake && ack:
		contract = HostProgressAcknowledgedStreaming
		ackPolicy = HostProgressAckRequired
	case callbacks && wake:
		contract = HostProgressUnacknowledgedStreaming
	case durable || follow:
		contract = HostProgressDurableFollowPoll
	case origin.Bound:
		contract = HostProgressKnownOriginReplay
	}

	return HostProgressTransportRecord{
		TransportContract: contract,
		AckPolicy:         ackPolicy,
		FallbackOrder: []string{
			HostProgressAcknowledgedStreaming,
			HostProgressUnacknowledgedStreaming,
			HostProgressDurableFollowPoll,
			HostProgressKnownOriginReplay,
			HostProgressNextInvocationReplay,
		},
		Limits: normalizeProgressLimits(limits),
		Stages: progressStages(contract, ack, callbacks, wake),
	}
}

func normalizeProgressLimits(limits HostProgressLimitDeclaration) HostProgressLimitRecord {
	record := HostProgressLimitRecord{
		MaxPayloadBytes:      boundedPositive(limits.MaxPayloadBytes, 0, 1<<20),
		MaxEnvelopeBytes:     boundedPositive(limits.MaxEnvelopeBytes, 0, 4<<20),
		MaxReceiptsPerMinute: boundedPositive(limits.MaxReceiptsPerMinute, 0, 6000),
		MaxOutstanding:       boundedPositive(limits.MaxOutstanding, 0, 10000),
	}
	record.Declared = record.MaxPayloadBytes > 0 || record.MaxEnvelopeBytes > 0 || record.MaxReceiptsPerMinute > 0 || record.MaxOutstanding > 0
	return record
}

func boundedPositive(value, fallback, max int) int {
	if value <= 0 {
		return fallback
	}
	if value > max {
		return max
	}
	return value
}

func progressStages(contract string, ack, callbacks, wake bool) []HostProgressStageRecord {
	stages := []HostProgressStageRecord{
		{Stage: HostProgressStageReceiptGeneration, Code: HostStageLocalOnly, EvidenceRequired: false, EvidenceKind: "progress-receipt"},
	}
	writeCode := HostStageReplayOnly
	if callbacks || contract == HostProgressDurableFollowPoll {
		writeCode = HostStageEvidenceRequired
	}
	stages = append(stages, HostProgressStageRecord{
		Stage:            HostProgressStageTransportWrite,
		Code:             writeCode,
		EvidenceRequired: writeCode == HostStageEvidenceRequired,
		EvidenceKind:     "transport-write",
	})
	acceptCode := HostStageUnsupported
	if callbacks {
		acceptCode = HostStageEvidenceRequired
	}
	stages = append(stages, HostProgressStageRecord{
		Stage:            HostProgressStageHostAcceptance,
		Code:             acceptCode,
		EvidenceRequired: acceptCode == HostStageEvidenceRequired,
		EvidenceKind:     "host-accepted",
	})
	visibleCode := HostStageUnsupported
	if ack {
		visibleCode = HostStageEvidenceRequired
	}
	stages = append(stages, HostProgressStageRecord{
		Stage:            HostProgressStageUserVisibility,
		Code:             visibleCode,
		EvidenceRequired: visibleCode == HostStageEvidenceRequired,
		EvidenceKind:     "host-visible",
	})
	ackCode := HostStageUnsupported
	if ack {
		ackCode = HostStageEvidenceRequired
	}
	stages = append(stages, HostProgressStageRecord{
		Stage:            HostProgressStageAcknowledgment,
		Code:             ackCode,
		EvidenceRequired: ackCode == HostStageEvidenceRequired,
		EvidenceKind:     "host-acknowledged",
	})
	wakeCode := HostStageUnsupported
	if wake {
		wakeCode = HostStageEvidenceRequired
	}
	stages = append(stages, HostProgressStageRecord{
		Stage:            HostProgressStageWakeUp,
		Code:             wakeCode,
		EvidenceRequired: wakeCode == HostStageEvidenceRequired,
		EvidenceKind:     "host-wake-up",
	})
	return stages
}

func BindHostRunOrigin(request HostRunOriginBindingRequest) HostRunOriginBinding {
	origin := request.Origin
	if strings.TrimSpace(origin.OpaqueID) == "" && strings.TrimSpace(origin.Kind) == "" && len(origin.Metadata) == 0 &&
		strings.TrimSpace(origin.SchemaVersion) == "" && len(origin.SupportedSchemaVersions) == 0 {
		return HostRunOriginBinding{Bound: false, Code: HostOriginAbsent, Redacted: true}
	}
	scope := normalizeOriginScope(request)
	if scope.ProjectID == "" || scope.DeliveryRunID == "" || scope.CorrelationID == "" {
		return HostRunOriginBinding{Bound: false, Code: ErrInvalidHostOriginScope, Redacted: true}
	}
	selectedSchema := selectOriginSchema(origin)
	if selectedSchema == "" {
		return HostRunOriginBinding{Bound: false, Code: ErrUnsupportedHostOriginSchemaVersion, Redacted: true}
	}
	kind := stableDiagnosticValue(origin.Kind, "opaque-origin")
	opaqueDigest := digestString(strings.TrimSpace(origin.OpaqueID))
	if opaqueDigest == "" {
		return HostRunOriginBinding{Bound: false, Code: ErrInvalidHostOriginMetadata, Redacted: true}
	}
	metadataDigest, metadataKeys, ok := normalizeOriginMetadata(origin.Metadata)
	if !ok {
		return HostRunOriginBinding{Bound: false, Code: ErrHostOriginMetadataTooLarge, Redacted: true}
	}
	bindingID := prefixedHostDigest("horigin", map[string]any{
		"schema_version":  HostRunOriginSchemaVersion,
		"project_id":      scope.ProjectID,
		"delivery_run_id": scope.DeliveryRunID,
		"correlation_id":  scope.CorrelationID,
		"origin_kind":     kind,
		"opaque_digest":   opaqueDigest,
		"metadata_digest": metadataDigest,
	})
	return HostRunOriginBinding{
		Bound:          true,
		Code:           HostOriginBound,
		SchemaVersion:  selectedSchema,
		BindingID:      bindingID,
		OriginKind:     kind,
		OriginRef:      opaqueDigest,
		ProjectID:      scope.ProjectID,
		DeliveryRunID:  scope.DeliveryRunID,
		CorrelationID:  scope.CorrelationID,
		MetadataDigest: metadataDigest,
		MetadataKeys:   metadataKeys,
		Redacted:       true,
		Limits: map[string]int{
			"max_metadata_keys":  maxHostOriginMetadataKeys,
			"max_metadata_runes": maxHostOriginMetadataRunes,
			"max_value_runes":    maxHostOriginValueRunes,
		},
	}
}

func selectOriginSchema(origin HostRunOriginDeclaration) string {
	supported := uniqueStrings(origin.SupportedSchemaVersions)
	if len(supported) == 0 && strings.TrimSpace(origin.SchemaVersion) != "" {
		supported = []string{strings.TrimSpace(origin.SchemaVersion)}
	}
	if len(supported) == 0 {
		return HostRunOriginSchemaVersion
	}
	for _, version := range supported {
		if version == HostRunOriginSchemaVersion {
			return HostRunOriginSchemaVersion
		}
	}
	return ""
}

type originScope struct {
	ProjectID     string
	DeliveryRunID string
	CorrelationID string
}

func normalizeOriginScope(request HostRunOriginBindingRequest) originScope {
	return originScope{
		ProjectID:     stableOriginID(request.ProjectID),
		DeliveryRunID: stableOriginID(request.DeliveryRunID),
		CorrelationID: stableOriginID(request.CorrelationID),
	}
}

func stableOriginID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, `/\`) || likelySecret(value) {
		return ""
	}
	if len([]rune(value)) > maxHostOriginValueRunes {
		return ""
	}
	return value
}

func normalizeOriginMetadata(metadata map[string]string) (string, []string, bool) {
	if len(metadata) > maxHostOriginMetadataKeys {
		return "", nil, false
	}
	redacted := map[string]string{}
	keys := make([]string, 0, len(metadata))
	totalRunes := 0
	for key, value := range metadata {
		key = stableDiagnosticValue(key, "redacted-key")
		if key == "redacted-key" {
			key = key + "-" + digestString(value)[:12]
		}
		if len([]rune(value)) > maxHostOriginMetadataRunes {
			return "", nil, false
		}
		value = stableOriginMetadataValue(value)
		totalRunes += len([]rune(key)) + len([]rune(value))
		if totalRunes > maxHostOriginMetadataRunes {
			return "", nil, false
		}
		redacted[key] = value
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(redacted) == 0 {
		return "", nil, true
	}
	return prefixedHostDigest("sha256", redacted), keys, true
}

func stableOriginMetadataValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.ContainsAny(value, `/\`) || likelySecret(value) {
		return "[redacted]"
	}
	runes := []rune(value)
	if len(runes) > maxHostOriginValueRunes {
		return string(runes[:maxHostOriginValueRunes])
	}
	return value
}

func digestString(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func prefixedHostDigest(prefix string, value any) string {
	payload, err := json.Marshal(value)
	if err != nil {
		return prefix + "_invalid"
	}
	sum := sha256.Sum256(payload)
	hexDigest := hex.EncodeToString(sum[:])
	if prefix == "sha256" {
		return "sha256:" + hexDigest
	}
	return prefix + "_" + hexDigest[:40]
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
	case HostLocalSubprocess, HostStdout, HostStderr, HostJSONOutput, HostTimeouts, HostCancellation, HostHooks,
		HostDurablePolling, HostResumableFollow, HostManagedBackgroundWork, HostCallbacks, HostWakeUp, HostAcknowledgment,
		HostDetachedSteering, HostDetachedCancellation, HostPayloadLimits, HostRateLimits:
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

func capabilityIsSupported(capabilities []HostCapabilityDeclaration, target HostCapability) bool {
	return hostCapabilitySupport(capabilities, target) == HostCapabilitySupported
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
