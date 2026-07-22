package noproviderdup_test

import (
	"testing"

	"github.com/jasonhnd/loopcoder/internal/noproviderdup"
)

func TestRemovedSurfacesDenied(t *testing.T) {
	for _, s := range []noproviderdup.Surface{
		noproviderdup.SurfaceInventory, noproviderdup.SurfaceRoutingWrite,
		noproviderdup.SurfaceFallbackRoute, noproviderdup.SurfaceRawSQLRepo,
	} {
		d := noproviderdup.Evaluate(s, noproviderdup.ActionWrite, false)
		if d.Allowed {
			t.Fatalf("%s write allowed", s)
		}
	}
}

func TestPreservedReaders(t *testing.T) {
	d := noproviderdup.Evaluate(noproviderdup.SurfacePinReader, noproviderdup.ActionRead, false)
	if !d.Allowed {
		t.Fatal(d)
	}
	d2 := noproviderdup.Evaluate(noproviderdup.SurfacePinReader, noproviderdup.ActionWrite, false)
	if d2.Allowed {
		t.Fatal("pin write denied")
	}
}

func TestOfficialAdapterFacade(t *testing.T) {
	d := noproviderdup.Evaluate(noproviderdup.SurfaceProcessInvoke, noproviderdup.ActionInvoke, false)
	if d.Allowed {
		t.Fatal("invoke without adapter")
	}
	d2 := noproviderdup.Evaluate(noproviderdup.SurfaceProcessInvoke, noproviderdup.ActionInvoke, true)
	if !d2.Allowed {
		t.Fatal(d2)
	}
}

func TestCLIDenied(t *testing.T) {
	if !noproviderdup.CLICallerDenied("route-write") {
		t.Fatal()
	}
	if noproviderdup.CLICallerDenied("doctor") {
		t.Fatal()
	}
}

func TestInventory(t *testing.T) {
	inv := noproviderdup.Inventory()
	if len(inv) < 8 {
		t.Fatal(len(inv))
	}
}
