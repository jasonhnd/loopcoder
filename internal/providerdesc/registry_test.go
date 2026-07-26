package providerdesc_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/providerdesc"
)

func t0() time.Time { return time.Date(2026, 7, 22, 18, 0, 0, 0, time.UTC) }

func TestRegisterAndObserveSuccess(t *testing.T) {
	reg := providerdesc.NewRegistry(t0)
	fake := providerdesc.NewFake()
	fake.Now = t0
	regd, err := reg.Register(fake)
	if err != nil || !regd.Eligible {
		t.Fatalf("%+v err=%v", regd, err)
	}
	obs, err := reg.Observe("fake", providerdesc.OpCatalog, nil)
	if err != nil || !obs.OK {
		t.Fatalf("%+v err=%v", obs, err)
	}
	if obs.Provenance.Source == "" || obs.Confidence == "" {
		t.Fatalf("missing envelope fields: %+v", obs)
	}
	if obs.Payload["models"] == "" {
		t.Fatal("catalog payload")
	}
}

func TestDuplicateAndVersionAndMismatch(t *testing.T) {
	reg := providerdesc.NewRegistry(t0)
	if _, err := reg.Register(providerdesc.NewFake()); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Register(providerdesc.NewFake()); !errors.Is(err, providerdesc.ErrDuplicate) && err == nil {
		t.Fatalf("err=%v", err)
	}
	if len(reg.List()) != 1 {
		t.Fatal(reg.List())
	}

	// invalid version via ValidateDescriptor path
	d := providerdesc.NewFake().Descriptor()
	d.Version = 0
	if err := providerdesc.ValidateDescriptor(d); err == nil {
		t.Fatal("expected version error")
	}
	d = providerdesc.NewFake().Descriptor()
	d.Operations = []providerdesc.Operation{providerdesc.OpDiscover}
	d.Models = []providerdesc.ModelEntry{{ID: "x"}}
	if err := providerdesc.ValidateDescriptor(d); err == nil {
		t.Fatal("expected models without catalog error")
	}
}

func TestUnsupportedAndBoundary(t *testing.T) {
	reg := providerdesc.NewRegistry(t0)
	fake := providerdesc.NewFake()
	fake.Mode = providerdesc.FakeUnsupported
	if _, err := reg.Register(fake); err != nil {
		t.Fatal(err)
	}
	_, err := reg.Observe("fake", providerdesc.OpInvoke, nil)
	if !errors.Is(err, providerdesc.ErrUnsupported) {
		t.Fatalf("err=%v", err)
	}
	// credential boundary
	reg3 := providerdesc.NewRegistry(t0)
	_, _ = reg3.Register(providerdesc.NewFake())
	_, err = reg3.Observe("fake", providerdesc.OpInvoke, map[string]string{"api_token": "x"})
	if !errors.Is(err, providerdesc.ErrBoundary) {
		t.Fatalf("err=%v", err)
	}
}

func TestConformanceSuite(t *testing.T) {
	m, err := providerdesc.RunConformance(t0)
	if err != nil {
		t.Fatal(err)
	}
	if m.Schema != providerdesc.SchemaConformance {
		t.Fatal(m.Schema)
	}
	if m.ActualCalls > m.MaxCalls {
		t.Fatalf("calls %d > max %d", m.ActualCalls, m.MaxCalls)
	}
	for _, v := range m.Vectors {
		if !v.Pass {
			t.Fatalf("vector %s failed: %s", v.Vector, v.Detail)
		}
	}
	if len(m.Vectors) < 10 {
		t.Fatalf("too few vectors: %d", len(m.Vectors))
	}
}

func TestInventoryDisposition(t *testing.T) {
	if !providerdesc.InventoryOK() {
		t.Fatal("inventory empty/invalid")
	}
	items := providerdesc.ExistingProviderInventory()
	found := false
	for _, it := range items {
		if it.Package == "internal/providerexec" && it.Disposition == "wrap" {
			found = true
		}
	}
	if !found {
		t.Fatalf("%+v", items)
	}
}

func TestFailedRegisterLeavesNoEntry(t *testing.T) {
	reg := providerdesc.NewRegistry(t0)
	d := providerdesc.NewFake().Descriptor()
	d.Version = 99
	// Can't easily inject bad fake without custom type — Register validates Descriptor()
	// Use a one-off
	bad := &badAdapter{d: d}
	_, err := reg.Register(bad)
	if err == nil || len(reg.List()) != 0 {
		t.Fatalf("err=%v list=%v", err, reg.List())
	}
}

type badAdapter struct{ d providerdesc.Descriptor }

func (b *badAdapter) Descriptor() providerdesc.Descriptor { return b.d }
func (b *badAdapter) Observe(providerdesc.Operation, map[string]string) (providerdesc.Observation, error) {
	return providerdesc.Observation{}, nil
}
