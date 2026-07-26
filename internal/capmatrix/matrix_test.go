package capmatrix_test

import (
	"testing"

	"github.com/jasonhnd/loopcoder/internal/capmatrix"
)

func TestMatrixHasPlatformAndUnsupported(t *testing.T) {
	m := capmatrix.Matrix()
	if len(m) < 10 {
		t.Fatalf("matrix too small: %d", len(m))
	}
}

func TestLookupProductAndAutonomous(t *testing.T) {
	c, ok := capmatrix.Lookup("plat-darwin-arm64")
	if !ok || !c.Supported || c.Evidence != capmatrix.TierProduct {
		t.Fatalf("%#v ok=%v", c, ok)
	}
	u, ok3 := capmatrix.Lookup("mode-autonomous")
	if !ok3 || u.Supported {
		t.Fatalf("autonomous must be unsupported: %#v", u)
	}
	ids := capmatrix.UnsupportedIDs()
	if len(ids) < 3 {
		t.Fatalf("unsupported ids: %v", ids)
	}
}

func TestDoctorCodes(t *testing.T) {
	codes := capmatrix.DoctorCodes()
	if len(codes) < 4 {
		t.Fatal(len(codes))
	}
	found := false
	for _, c := range codes {
		if c.Code == "use_ordinary_dev" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing use_ordinary_dev")
	}
}

func TestByArea(t *testing.T) {
	p := capmatrix.ByArea("provider")
	if len(p) < 3 {
		t.Fatalf("providers=%d", len(p))
	}
}
