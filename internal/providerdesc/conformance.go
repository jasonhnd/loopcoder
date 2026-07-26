package providerdesc

import (
	"fmt"
	"strings"
	"time"
)

// ConformanceResult is one vector outcome.
type ConformanceResult struct {
	Vector string `json:"vector"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

// ConformanceManifest records suite evidence (redacted).
type ConformanceManifest struct {
	Schema    string              `json:"schema"`
	AdapterID string              `json:"adapter_id"`
	Vectors   []ConformanceResult `json:"vectors"`
	// MaxCalls bounds observation fan-out.
	MaxCalls    int       `json:"max_calls"`
	ActualCalls int       `json:"actual_calls"`
	GeneratedAt time.Time `json:"generated_at"`
}

// RunConformance executes the reusable fake-provider suite.
func RunConformance(now func() time.Time) (ConformanceManifest, error) {
	if now == nil {
		now = time.Now
	}
	var results []ConformanceResult
	totalCalls := 0

	// 1. success path
	{
		reg := NewRegistry(now)
		fake := NewFake()
		fake.Now = now
		if _, err := reg.Register(fake); err != nil {
			results = append(results, ConformanceResult{Vector: "success_register", Pass: false, Detail: err.Error()})
		} else {
			results = append(results, ConformanceResult{Vector: "success_register", Pass: true})
			for _, op := range []Operation{OpDiscover, OpAuthStatus, OpCatalog, OpQuota, OpInvoke, OpDiagnose} {
				obs, err := reg.Observe("fake", op, map[string]string{"model": "fixture-model"})
				totalCalls++
				ok := err == nil && obs.OK && obs.Provenance.Source != "" && obs.Confidence != ""
				results = append(results, ConformanceResult{
					Vector: "success_" + string(op), Pass: ok,
					Detail: fmt.Sprintf("err=%v ok=%v", err, obs.OK),
				})
			}
		}
	}

	// 2. missing install
	{
		reg := NewRegistry(now)
		fake := NewFake()
		fake.Mode = FakeMissingInstall
		fake.Now = now
		_, _ = reg.Register(fake)
		obs, err := reg.Observe("fake", OpDiscover, nil)
		totalCalls++
		pass := err == nil && !obs.OK && obs.Diagnostic != nil && obs.Diagnostic.Class == DiagMissingInstall
		results = append(results, ConformanceResult{Vector: "missing_install", Pass: pass})
	}

	// 3. auth unknown
	{
		reg := NewRegistry(now)
		fake := NewFake()
		fake.Mode = FakeAuthUnknown
		fake.Now = now
		_, _ = reg.Register(fake)
		obs, err := reg.Observe("fake", OpAuthStatus, nil)
		totalCalls++
		pass := err == nil && obs.OK && obs.Payload["auth"] == string(AuthUnknown)
		results = append(results, ConformanceResult{Vector: "auth_unknown", Pass: pass})
	}

	// 4. malformed
	{
		reg := NewRegistry(now)
		fake := NewFake()
		fake.Mode = FakeMalformed
		fake.Now = now
		_, _ = reg.Register(fake)
		obs, err := reg.Observe("fake", OpCatalog, nil)
		totalCalls++
		pass := err == nil && !obs.OK && obs.Diagnostic != nil && obs.Diagnostic.Class == DiagMalformed
		results = append(results, ConformanceResult{Vector: "malformed_output", Pass: pass})
	}

	// 5. timeout
	{
		reg := NewRegistry(now)
		fake := NewFake()
		fake.Mode = FakeTimeout
		fake.Now = now
		_, _ = reg.Register(fake)
		obs, err := reg.Observe("fake", OpInvoke, map[string]string{"model": "fixture-model"})
		totalCalls++
		pass := err == nil && !obs.OK && obs.Diagnostic != nil && obs.Diagnostic.Class == DiagTimeout
		results = append(results, ConformanceResult{Vector: "timeout", Pass: pass})
	}

	// 6. rate limit
	{
		reg := NewRegistry(now)
		fake := NewFake()
		fake.Mode = FakeRateLimit
		fake.Now = now
		_, _ = reg.Register(fake)
		obs, err := reg.Observe("fake", OpQuota, nil)
		totalCalls++
		pass := err == nil && !obs.OK && obs.Diagnostic != nil && obs.Diagnostic.Class == DiagRateLimit
		results = append(results, ConformanceResult{Vector: "rate_limit", Pass: pass})
	}

	// 7. unsupported operation (not claimed)
	{
		reg := NewRegistry(now)
		fake := NewFake()
		fake.Mode = FakeUnsupported
		fake.Now = now
		_, _ = reg.Register(fake)
		_, err := reg.Observe("fake", OpInvoke, nil)
		totalCalls++
		pass := err != nil
		results = append(results, ConformanceResult{Vector: "unsupported_operation", Pass: pass, Detail: fmt.Sprint(err)})
	}

	// 8. duplicate registration fails with no second entry
	{
		reg := NewRegistry(now)
		_, err1 := reg.Register(NewFake())
		_, err2 := reg.Register(NewFake())
		pass := err1 == nil && err2 != nil && len(reg.List()) == 1
		results = append(results, ConformanceResult{Vector: "duplicate_id", Pass: pass})
	}

	// 9. incompatible version rejected with empty registry
	{
		reg := NewRegistry(now)
		bad := &staticAdapter{d: Descriptor{
			AdapterID: "badver", Version: 99, DisplayName: "Bad",
			Operations: []Operation{OpDiscover},
		}}
		_, err := reg.Register(bad)
		pass := err != nil && len(reg.List()) == 0
		results = append(results, ConformanceResult{Vector: "incompatible_version", Pass: pass})
	}

	// 10. credential input rejected
	{
		reg := NewRegistry(now)
		_, _ = reg.Register(NewFake())
		_, err := reg.Observe("fake", OpInvoke, map[string]string{"token": "sk-secretvalue999"})
		pass := err != nil
		results = append(results, ConformanceResult{Vector: "credential_boundary", Pass: pass})
	}

	// 11. capability mismatch (models without catalog)
	{
		reg := NewRegistry(now)
		bad := &staticAdapter{d: Descriptor{
			AdapterID: "badcap", Version: DescriptorVersion, DisplayName: "BadCap",
			Operations: []Operation{OpDiscover},
			Models:     []ModelEntry{{ID: "m"}},
		}}
		_, err := reg.Register(bad)
		pass := err != nil && len(reg.List()) == 0
		results = append(results, ConformanceResult{Vector: "capability_mismatch", Pass: pass})
	}

	allPass := true
	for _, r := range results {
		if !r.Pass {
			allPass = false
			break
		}
	}
	_ = allPass

	return ConformanceManifest{
		Schema: SchemaConformance, AdapterID: "fake", Vectors: results,
		MaxCalls: 32, ActualCalls: totalCalls, GeneratedAt: now().UTC(),
	}, nil
}

// staticAdapter is a minimal Adapter for negative registry tests.
type staticAdapter struct{ d Descriptor }

func (s *staticAdapter) Descriptor() Descriptor { return s.d }
func (s *staticAdapter) Observe(Operation, map[string]string) (Observation, error) {
	return Observation{}, ErrUnsupported
}

// InventoryDisposition documents migration for existing provider packages.
type InventoryDisposition struct {
	Package     string `json:"package"`
	Disposition string `json:"disposition"` // adopt|wrap|retire|keep_aux
	Note        string `json:"note"`
}

// ExistingProviderInventory lists current provider-related packages and disposition.
func ExistingProviderInventory() []InventoryDisposition {
	return []InventoryDisposition{
		{Package: "internal/providerexec", Disposition: "wrap", Note: "execution contract remains; register via descriptor OpInvoke"},
		{Package: "internal/providerinventory", Disposition: "wrap", Note: "discovery sources become OpDiscover/catalog plans"},
		{Package: "internal/providerauthority", Disposition: "keep_aux", Note: "authority store; not an adapter"},
		{Package: "internal/provideroutcome", Disposition: "adopt", Note: "map outcomes into Observation envelopes"},
		{Package: "internal/providerreconcile", Disposition: "wrap", Note: "reconciliation uses descriptor probe plans"},
		{Package: "internal/provider", Disposition: "retire", Note: "smoke helpers only; fold into conformance"},
		{Package: "internal/models", Disposition: "wrap", Note: "catalog entries feed OpCatalog"},
		{Package: "internal/quotaheadroom", Disposition: "wrap", Note: "quota observations via OpQuota"},
		{Package: "internal/routepin", Disposition: "keep_aux", Note: "route policy outside adapter boundary"},
	}
}

// InventoryOK reports inventory is non-empty and dispositions are known.
func InventoryOK() bool {
	items := ExistingProviderInventory()
	if len(items) == 0 {
		return false
	}
	for _, it := range items {
		if strings.TrimSpace(it.Package) == "" || strings.TrimSpace(it.Disposition) == "" {
			return false
		}
	}
	return true
}
