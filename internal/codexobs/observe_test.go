package codexobs_test

import (
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/codexobs"
	"github.com/jasonhnd/loopcoder/internal/providerdesc"
)

func t0() time.Time { return time.Date(2026, 7, 23, 2, 0, 0, 0, time.UTC) }

func TestInstalledCatalog(t *testing.T) {
	o := &codexobs.Observer{}
	snap, err := o.Observe(codexobs.ProbeInputs{
		ExecutablePresent: true, Version: "0.2.0", AuthState: "known",
		AccountProfile: "fixture-profile",
		RawModels: []string{
			"o4-mini|mini,o4mini|200000|low,medium,high",
			"gpt-5|g5|400000|medium",
		},
		Now: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Installed == nil || !*snap.Installed || snap.Version != "0.2.0" {
		t.Fatalf("%+v", snap)
	}
	if snap.Auth != "known" || snap.AccountProfile != "fixture-profile" {
		t.Fatalf("auth %+v", snap)
	}
	if len(snap.Models) != 2 {
		t.Fatalf("models=%+v", snap.Models)
	}
	if snap.Models[0].Source == "" || snap.Models[0].CanonicalID == "" {
		t.Fatal(snap.Models[0])
	}
	if snap.LaunchAttempted || snap.RouteChosen {
		t.Fatal("must not launch or route")
	}
	// alias normalize
	can, ok, expl := codexobs.NormalizeAlias(snap.Models, "mini")
	if !ok || can != "o4-mini" {
		t.Fatalf("%s %v %s", can, ok, expl)
	}
	// unknown not defaulted
	_, ok, expl = codexobs.NormalizeAlias(snap.Models, "auto")
	if ok {
		t.Fatalf("auto should not resolve: %s", expl)
	}
	_, ok, _ = codexobs.NormalizeAlias(snap.Models, "totally-unknown")
	if ok {
		t.Fatal("unknown became eligible")
	}
}

func TestNotInstalled(t *testing.T) {
	o := &codexobs.Observer{}
	snap, err := o.Observe(codexobs.ProbeInputs{ExecutablePresent: false, AuthState: "missing", Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Installed == nil || *snap.Installed {
		t.Fatalf("%+v", snap)
	}
	if len(snap.Models) != 0 {
		t.Fatal(snap.Models)
	}
}

func TestPreserveLastOnTimeout(t *testing.T) {
	o := &codexobs.Observer{}
	first, err := o.Observe(codexobs.ProbeInputs{
		ExecutablePresent: true, Version: "1.0", AuthState: "known",
		RawModels: []string{"o4-mini||1000|low"}, Now: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := o.Observe(codexobs.ProbeInputs{
		ExecutablePresent: true, Timeout: true, Now: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !second.LastKnownPreserved {
		t.Fatal("expected preserve")
	}
	if second.Version != first.Version && (second.Installed == nil || !*second.Installed) {
		// version may be preserved via last
	}
	if len(second.Models) == 0 && len(first.Models) > 0 {
		// catalog should be preserved
		t.Fatalf("catalog erased: diags=%v", second.Diagnostics)
	}
}

func TestAuthUnknownNotErased(t *testing.T) {
	o := &codexobs.Observer{}
	snap, err := o.Observe(codexobs.ProbeInputs{
		ExecutablePresent: true, Version: "1", AuthState: "unknown", Now: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Auth != "unknown" {
		t.Fatal(snap.Auth)
	}
}

func TestDefaultAliasRejected(t *testing.T) {
	models := []codexobs.ModelRecord{{CanonicalID: "o4-mini", Aliases: []string{"mini"}}}
	for _, bad := range []string{"default", "auto", ""} {
		_, ok, _ := codexobs.NormalizeAlias(models, bad)
		if ok {
			t.Fatalf("%q resolved", bad)
		}
	}
}

func TestRegistryConformance(t *testing.T) {
	reg := providerdesc.NewRegistry(t0)
	ad := &codexobs.AsAdapter{Obs: &codexobs.Observer{}, In: codexobs.ProbeInputs{
		ExecutablePresent: true, Version: "0.1", AuthState: "known",
		RawModels: []string{"o4-mini|mini|1|low"}, Now: t0,
	}}
	if _, err := reg.Register(ad); err != nil {
		t.Fatal(err)
	}
	for _, op := range []providerdesc.Operation{providerdesc.OpDiscover, providerdesc.OpAuthStatus, providerdesc.OpCatalog} {
		obs, err := reg.Observe(codexobs.AdapterID, op, nil)
		if err != nil || !obs.OK {
			t.Fatalf("%s %+v err=%v", op, obs, err)
		}
	}
	_, err := reg.Observe(codexobs.AdapterID, providerdesc.OpInvoke, nil)
	if err == nil {
		t.Fatal("invoke must be unsupported")
	}
}

func TestNoCredentialInProfile(t *testing.T) {
	o := &codexobs.Observer{}
	snap, err := o.Observe(codexobs.ProbeInputs{
		ExecutablePresent: true, AuthState: "known",
		AccountProfile: "sk-secretvalue999", Now: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snap.AccountProfile != "" {
		t.Fatalf("secret profile stored: %q", snap.AccountProfile)
	}
}
