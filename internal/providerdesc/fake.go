package providerdesc

import (
	"strings"
	"sync"
	"time"
)

// FakeMode selects fake adapter behavior for conformance.
type FakeMode string

const (
	FakeOK             FakeMode = "ok"
	FakeMissingInstall FakeMode = "missing_install"
	FakeAuthUnknown    FakeMode = "auth_unknown"
	FakeMalformed      FakeMode = "malformed"
	FakeTimeout        FakeMode = "timeout"
	FakeRateLimit      FakeMode = "rate_limit"
	FakeUnsupported    FakeMode = "unsupported" // claims nothing extra; used for op rejection tests
)

// FakeAdapter is the reusable conformance kit provider.
type FakeAdapter struct {
	mu   sync.Mutex
	Mode FakeMode
	// Now injects observation time.
	Now func() time.Time
	// CallCount for bounds checks.
	CallCount int
}

// NewFake returns a healthy fake with full SPI ops.
func NewFake() *FakeAdapter {
	return &FakeAdapter{Mode: FakeOK, Now: time.Now}
}

func (f *FakeAdapter) now() time.Time {
	if f.Now != nil {
		return f.Now().UTC()
	}
	return time.Now().UTC()
}

// Descriptor implements Adapter.
func (f *FakeAdapter) Descriptor() Descriptor {
	ops := []Operation{OpDiscover, OpAuthStatus, OpCatalog, OpQuota, OpInvoke, OpDiagnose}
	unsupported := []Operation{}
	models := []ModelEntry{
		{ID: "fixture-model", DisplayName: "Fixture", Capabilities: []string{"chat"}},
	}
	if f.Mode == FakeUnsupported {
		ops = []Operation{OpDiscover}
		unsupported = []Operation{OpAuthStatus, OpCatalog, OpQuota, OpInvoke, OpDiagnose}
		models = nil
	}
	return Descriptor{
		Schema: SchemaDescriptor, AdapterID: "fake", Version: DescriptorVersion,
		DisplayName: "Fake Conformance Provider",
		Identity:    Identity{InstallID: "fake-install", AccountID: "fixture-account", Present: f.Mode != FakeMissingInstall},
		Operations:  ops, Unsupported: unsupported,
		ProbePlans: []ProbePlan{{Name: "version", Timeout: time.Second}, {Name: "auth", Timeout: 2 * time.Second, Optional: true}},
		Models:     models,
		Notes:      "conformance-only; no network",
	}
}

// Observe implements Adapter.
func (f *FakeAdapter) Observe(op Operation, in map[string]string) (Observation, error) {
	f.mu.Lock()
	f.CallCount++
	mode := f.Mode
	f.mu.Unlock()

	base := Observation{
		Schema: SchemaObservation, AdapterID: "fake", Operation: op,
		Provenance: Provenance{Source: "fake_fixture", ObservedAt: f.now(), Freshness: "fresh"},
	}

	// Mode-wide faults
	switch mode {
	case FakeMissingInstall:
		if op == OpDiscover || op == OpInvoke {
			base.OK = false
			base.Confidence = ConfidenceHigh
			base.Diagnostic = &Diagnostic{Schema: SchemaDiagnostic, Class: DiagMissingInstall, Message: "fixture not installed", Code: "E_INSTALL"}
			return base, nil
		}
	case FakeTimeout:
		base.OK = false
		base.Confidence = ConfidenceMedium
		base.Diagnostic = &Diagnostic{Schema: SchemaDiagnostic, Class: DiagTimeout, Message: "probe timed out", Code: "E_TIMEOUT"}
		return base, nil
	case FakeRateLimit:
		base.OK = false
		base.Confidence = ConfidenceHigh
		base.Diagnostic = &Diagnostic{Schema: SchemaDiagnostic, Class: DiagRateLimit, Message: "rate limited", Code: "E_RATE"}
		return base, nil
	case FakeMalformed:
		if op == OpCatalog || op == OpInvoke {
			base.OK = false
			base.Confidence = ConfidenceLow
			base.Diagnostic = &Diagnostic{Schema: SchemaDiagnostic, Class: DiagMalformed, Message: "malformed fixture output", Code: "E_MALFORMED"}
			return base, nil
		}
	case FakeAuthUnknown:
		if op == OpAuthStatus {
			base.OK = true
			base.Confidence = ConfidenceLow
			base.Payload = map[string]string{"auth": string(AuthUnknown)}
			base.Diagnostic = &Diagnostic{Schema: SchemaDiagnostic, Class: DiagAuthUnknown, Message: "auth not probed", Code: "E_AUTH_UNKNOWN"}
			return base, nil
		}
	}

	switch op {
	case OpDiscover:
		base.OK = true
		base.Confidence = ConfidenceHigh
		base.Payload = map[string]string{"present": "true", "install_id": "fake-install"}
	case OpAuthStatus:
		base.OK = true
		base.Confidence = ConfidenceHigh
		base.Payload = map[string]string{"auth": string(AuthKnownOK)}
	case OpCatalog:
		base.OK = true
		base.Confidence = ConfidenceHigh
		base.Payload = map[string]string{"models": "fixture-model", "count": "1"}
	case OpQuota:
		base.OK = true
		base.Confidence = ConfidenceMedium
		base.Payload = map[string]string{"headroom": "sufficient", "window": "fixture"}
	case OpInvoke:
		// Never accept secrets in input (registry already checks).
		model := in["model"]
		if model == "" {
			model = "fixture-model"
		}
		base.OK = true
		base.Confidence = ConfidenceHigh
		base.Payload = map[string]string{"exit": "0", "model": model, "completion": "fixture_ok"}
	case OpDiagnose:
		base.OK = true
		base.Confidence = ConfidenceHigh
		base.Payload = map[string]string{"health": "ok", "redaction": "on"}
	default:
		base.OK = false
		base.Confidence = ConfidenceNone
		base.Diagnostic = &Diagnostic{Schema: SchemaDiagnostic, Class: DiagUnsupported, Message: "unknown op"}
		return base, ErrUnsupported
	}
	// Redact any accidental absolute paths in payload values.
	for k, v := range base.Payload {
		if strings.Contains(v, "/Users/") || strings.Contains(v, "HOME=") {
			delete(base.Payload, k)
		}
	}
	return base, nil
}
