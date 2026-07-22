package routecanary

import (
	"testing"
	"time"
)

func TestSmartRoutingCanaryMatrix(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	m, err := Run(now, "67463d7e45dc29166595b024d8a07f040b4dcf56")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Passed {
		for _, s := range m.Scenarios {
			if !s.Passed {
				t.Errorf("FAIL %s: %s", s.Name, s.Detail)
			}
		}
		t.Fatalf("canary failed digest=%s", m.Digest)
	}
	if len(m.Scenarios) < 10 {
		t.Fatalf("scenario count %d", len(m.Scenarios))
	}
	if m.Digest == "" || m.Schema != SchemaManifest {
		t.Fatalf("%+v", m)
	}
	// deterministic
	m2, _ := Run(now, "67463d7e45dc29166595b024d8a07f040b4dcf56")
	if m.Digest != m2.Digest {
		t.Fatal("non-deterministic manifest")
	}
	// resource notes
	if len(m.ResourceNotes) < 3 {
		t.Fatal("resource notes")
	}
}

func TestCanaryNoZeroNow(t *testing.T) {
	_, err := Run(time.Time{}, "")
	if err == nil {
		t.Fatal("expected error")
	}
}
