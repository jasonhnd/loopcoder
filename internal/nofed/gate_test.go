package nofed_test

import (
	"testing"

	"github.com/jasonhnd/loopcoder/internal/nofed"
)

func TestDenyFederationAndLeases(t *testing.T) {
	for _, sys := range []nofed.System{
		nofed.SysFederation, nofed.SysCrossMacLease, nofed.SysStateBranch, nofed.SysStateDBPushPull,
	} {
		d := nofed.Evaluate(sys, nofed.ActionWrite)
		if d.Allowed {
			t.Fatalf("%s allowed", sys)
		}
	}
}

func TestPreserveP5AndRehydrate(t *testing.T) {
	for _, sys := range []nofed.System{nofed.SysWorkGraph, nofed.SysNativeChild, nofed.SysGitHubRehydrate} {
		d := nofed.Evaluate(sys, nofed.ActionExecute)
		if !d.Allowed {
			t.Fatal(sys, d)
		}
	}
}

func TestCLIAndMatrix(t *testing.T) {
	if !nofed.CLIDenied("federate") {
		t.Fatal()
	}
	m := nofed.CapabilityMatrix()
	if len(m) < 5 {
		t.Fatal()
	}
}
