package providerdesc

import (
	"fmt"
	"strings"
	"time"
)

const (
	SchemaDescriptor  = "loopcoder.provider.descriptor.v1"
	SchemaObservation = "loopcoder.provider.observation.v1"
	SchemaDiagnostic  = "loopcoder.provider.diagnostic.v1"
	SchemaConformance = "loopcoder.provider.conformance.v1"
	// DescriptorVersion is the SPI version this package accepts.
	DescriptorVersion = 1
)

// Operation is a capability a descriptor may claim.
type Operation string

const (
	OpDiscover   Operation = "discover"
	OpAuthStatus Operation = "auth_status"
	OpCatalog    Operation = "catalog"
	OpQuota      Operation = "quota"
	OpInvoke     Operation = "invoke"
	OpDiagnose   Operation = "diagnose"
)

// Confidence is observation confidence.
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
	ConfidenceNone   Confidence = "none"
)

// AuthState is redacted auth observation (never tokens).
type AuthState string

const (
	AuthKnownOK     AuthState = "ok"
	AuthUnknown     AuthState = "unknown"
	AuthMissing     AuthState = "missing"
	AuthExpired     AuthState = "expired"
	AuthUnavailable AuthState = "unavailable"
)

// DiagnosticClass is a typed adapter diagnostic.
type DiagnosticClass string

const (
	DiagNone           DiagnosticClass = ""
	DiagMissingInstall DiagnosticClass = "missing_install"
	DiagAuthUnknown    DiagnosticClass = "auth_unknown"
	DiagMalformed      DiagnosticClass = "malformed_output"
	DiagTimeout        DiagnosticClass = "timeout"
	DiagRateLimit      DiagnosticClass = "rate_limit"
	DiagUnsupported    DiagnosticClass = "unsupported_operation"
	DiagInternal       DiagnosticClass = "internal"
)

// Diagnostic is a redacted typed error/status envelope.
type Diagnostic struct {
	Schema  string          `json:"schema"`
	Class   DiagnosticClass `json:"class"`
	Message string          `json:"message"`
	// Code is adapter-stable; never includes secrets.
	Code string `json:"code,omitempty"`
}

// Provenance names the observation source without host paths.
type Provenance struct {
	Source     string    `json:"source"` // e.g. fake_cli, fixture_catalog
	ObservedAt time.Time `json:"observed_at"`
	// Freshness is age bound metadata only.
	Freshness string `json:"freshness,omitempty"` // fresh|stale|unknown
}

// Observation is the shared envelope for discovery/catalog/quota/invoke results.
type Observation struct {
	Schema     string      `json:"schema"`
	AdapterID  string      `json:"adapter_id"`
	Operation  Operation   `json:"operation"`
	OK         bool        `json:"ok"`
	Confidence Confidence  `json:"confidence"`
	Provenance Provenance  `json:"provenance"`
	Diagnostic *Diagnostic `json:"diagnostic,omitempty"`
	// Payload is operation-specific redacted fields only.
	Payload map[string]string `json:"payload,omitempty"`
}

// ModelEntry is one catalog model (no pricing secrets).
type ModelEntry struct {
	ID           string   `json:"id"`
	DisplayName  string   `json:"display_name,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// ProbePlan is a bounded observation plan step name (no executable payload).
type ProbePlan struct {
	Name     string        `json:"name"`
	Timeout  time.Duration `json:"timeout"`
	Optional bool          `json:"optional"`
}

// Identity is installation/account identity without credentials.
type Identity struct {
	// InstallID is a synthetic or redacted install marker (never path/token).
	InstallID string `json:"install_id,omitempty"`
	// AccountID is a redacted account handle if known.
	AccountID string `json:"account_id,omitempty"`
	// Present means the adapter believes the tool/runtime is installed.
	Present bool `json:"present"`
}

// Descriptor is the versioned SPI registration document.
type Descriptor struct {
	Schema      string      `json:"schema"`
	AdapterID   string      `json:"adapter_id"`
	Version     int         `json:"version"`
	DisplayName string      `json:"display_name"`
	Identity    Identity    `json:"identity"`
	Operations  []Operation `json:"operations"`
	// Unsupported lists ops explicitly not supported.
	Unsupported []Operation  `json:"unsupported,omitempty"`
	ProbePlans  []ProbePlan  `json:"probe_plans,omitempty"`
	Models      []ModelEntry `json:"models,omitempty"`
	// Notes are non-secret migration/disposition text.
	Notes string `json:"notes,omitempty"`
}

// ValidateDescriptor checks structural rules before registry eligibility.
func ValidateDescriptor(d Descriptor) error {
	if d.Schema != "" && d.Schema != SchemaDescriptor {
		return fmt.Errorf("providerdesc: schema %q", d.Schema)
	}
	id := strings.TrimSpace(strings.ToLower(d.AdapterID))
	if id == "" || strings.ContainsAny(id, " \t/") {
		return fmt.Errorf("providerdesc: invalid adapter_id")
	}
	if d.Version != DescriptorVersion {
		return fmt.Errorf("providerdesc: incompatible descriptor version %d (want %d)", d.Version, DescriptorVersion)
	}
	if d.DisplayName == "" {
		return fmt.Errorf("providerdesc: display_name required")
	}
	if len(d.Operations) == 0 {
		return fmt.Errorf("providerdesc: operations required")
	}
	seen := map[Operation]bool{}
	for _, op := range d.Operations {
		if !validOp(op) {
			return fmt.Errorf("providerdesc: unknown operation %q", op)
		}
		if seen[op] {
			return fmt.Errorf("providerdesc: duplicate operation %q", op)
		}
		seen[op] = true
	}
	for _, op := range d.Unsupported {
		if !validOp(op) {
			return fmt.Errorf("providerdesc: unknown unsupported %q", op)
		}
		if seen[op] {
			return fmt.Errorf("providerdesc: capability/result mismatch: %q claimed and unsupported", op)
		}
	}
	// Explicit unsupported for all non-claimed known ops is not required, but
	// if Models claimed then catalog should be claimed.
	if len(d.Models) > 0 && !seen[OpCatalog] {
		return fmt.Errorf("providerdesc: models present without catalog capability")
	}
	// Identity must not look like a secret or path.
	if strings.Contains(d.Identity.InstallID, "/") || strings.Contains(d.Identity.InstallID, "\\") {
		return fmt.Errorf("providerdesc: install_id must not be a path")
	}
	if looksSecret(d.Identity.AccountID) || looksSecret(d.Identity.InstallID) {
		return fmt.Errorf("providerdesc: identity looks like a secret")
	}
	return nil
}

func validOp(op Operation) bool {
	switch op {
	case OpDiscover, OpAuthStatus, OpCatalog, OpQuota, OpInvoke, OpDiagnose:
		return true
	default:
		return false
	}
}

func looksSecret(s string) bool {
	ls := strings.ToLower(s)
	return strings.HasPrefix(ls, "ghp_") || strings.HasPrefix(ls, "sk-") || strings.Contains(ls, "token=")
}
