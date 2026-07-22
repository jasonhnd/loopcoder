package exampleprovider

import (
	"time"

	"github.com/jasonhnd/loopcoder/internal/providerdesc"
)

const AdapterID = "example"

// Adapter is the synthetic fixture provider.
type Adapter struct {
	Now func() time.Time
}

func (a *Adapter) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

// Descriptor implements providerdesc.Adapter.
func (a *Adapter) Descriptor() providerdesc.Descriptor {
	return providerdesc.Descriptor{
		Schema: providerdesc.SchemaDescriptor, AdapterID: AdapterID,
		Version: providerdesc.DescriptorVersion, DisplayName: "Example Synthetic Provider",
		Identity: providerdesc.Identity{InstallID: "example-fixture", Present: true, AccountID: "fixture"},
		Operations: []providerdesc.Operation{
			providerdesc.OpDiscover, providerdesc.OpAuthStatus,
			providerdesc.OpCatalog, providerdesc.OpQuota, providerdesc.OpInvoke, providerdesc.OpDiagnose,
		},
		ProbePlans: []providerdesc.ProbePlan{{Name: "fixture", Timeout: time.Second}},
		Models: []providerdesc.ModelEntry{
			{ID: "example-model", DisplayName: "Example", Capabilities: []string{"chat"}},
		},
		Notes: "test-only; never production",
	}
}

// Observe implements providerdesc.Adapter.
func (a *Adapter) Observe(op providerdesc.Operation, in map[string]string) (providerdesc.Observation, error) {
	obs := providerdesc.Observation{
		Schema: providerdesc.SchemaObservation, AdapterID: AdapterID, Operation: op,
		OK: true, Confidence: providerdesc.ConfidenceHigh,
		Provenance: providerdesc.Provenance{Source: "example_fixture", ObservedAt: a.now(), Freshness: "fresh"},
		Payload:    map[string]string{},
	}
	switch op {
	case providerdesc.OpDiscover:
		obs.Payload["present"] = "true"
	case providerdesc.OpAuthStatus:
		obs.Payload["auth"] = "ok"
	case providerdesc.OpCatalog:
		obs.Payload["models"] = "example-model"
	case providerdesc.OpQuota:
		obs.Payload["headroom"] = "sufficient"
	case providerdesc.OpInvoke:
		model := in["model"]
		if model == "" {
			model = "example-model"
		}
		obs.Payload["exit"] = "0"
		obs.Payload["model"] = model
	case providerdesc.OpDiagnose:
		obs.Payload["health"] = "ok"
	default:
		obs.OK = false
		obs.Diagnostic = &providerdesc.Diagnostic{
			Schema: providerdesc.SchemaDiagnostic, Class: providerdesc.DiagUnsupported, Message: "unsupported",
		}
		return obs, providerdesc.ErrUnsupported
	}
	return obs, nil
}
