package providerkit_test

import (
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/exampleprovider"
	"github.com/jasonhnd/loopcoder/internal/providerdesc"
	"github.com/jasonhnd/loopcoder/internal/providerkit"
)

func t0() time.Time { return time.Date(2026, 7, 23, 14, 0, 0, 0, time.UTC) }

func TestSyntheticProviderConformance(t *testing.T) {
	reg := providerdesc.NewRegistry(t0)
	allow := providerkit.NewAllowlist("example")
	ad := &exampleprovider.Adapter{Now: t0}
	regd, err := providerkit.RegisterSafe(reg, allow, ad, false)
	if err != nil || !regd.Eligible {
		t.Fatalf("%+v err=%v", regd, err)
	}
	for _, op := range []providerdesc.Operation{
		providerdesc.OpDiscover, providerdesc.OpAuthStatus, providerdesc.OpCatalog,
		providerdesc.OpQuota, providerdesc.OpInvoke, providerdesc.OpDiagnose,
	} {
		obs, err := reg.Observe("example", op, map[string]string{"model": "example-model"})
		if err != nil || !obs.OK {
			t.Fatalf("%s %+v err=%v", op, obs, err)
		}
	}
}

func TestNoCustomerRepoAutoload(t *testing.T) {
	reg := providerdesc.NewRegistry(t0)
	allow := providerkit.NewAllowlist("example")
	_, err := providerkit.RegisterSafe(reg, allow, &exampleprovider.Adapter{Now: t0}, true)
	if err == nil {
		t.Fatal("expected forbid")
	}
	if len(reg.List()) != 0 {
		t.Fatal(reg.List())
	}
}

func TestNotAllowlisted(t *testing.T) {
	reg := providerdesc.NewRegistry(t0)
	allow := providerkit.NewAllowlist("codex")
	_, err := providerkit.RegisterSafe(reg, allow, &exampleprovider.Adapter{Now: t0}, false)
	if err == nil {
		t.Fatal("expected allowlist fail")
	}
}

func TestVersionPolicy(t *testing.T) {
	if err := providerkit.VersionPolicy(1); err != nil {
		t.Fatal(err)
	}
	if err := providerkit.VersionPolicy(99); err == nil {
		t.Fatal("expected version fail")
	}
}

func TestChecklist(t *testing.T) {
	ok, miss := providerkit.ValidateChecklist([]string{"auth_ownership"})
	if ok || len(miss) == 0 {
		t.Fatal("expected missing")
	}
	ok, miss = providerkit.ValidateChecklist(providerkit.DefaultChecklist().Required)
	if !ok || len(miss) != 0 {
		t.Fatalf("%v %v", ok, miss)
	}
}

func TestSupportModelDocs(t *testing.T) {
	s := providerkit.DefaultSupportModel()
	if len(s.OfficialBuiltIn) < 5 || s.DeferredPacks == "" {
		t.Fatalf("%+v", s)
	}
}
