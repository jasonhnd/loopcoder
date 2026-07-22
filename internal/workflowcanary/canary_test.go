package workflowcanary

import (
	"testing"
	"time"
)

func TestP5BoundedWorkflowCanary(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	m, err := Run(now, "3b31ff7879b44c8cb9deb80cdfa37465436964a5")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Passed {
		for _, s := range m.Scenarios {
			if !s.Passed {
				t.Errorf("FAIL %s: %s", s.Name, s.Detail)
			}
		}
		t.Fatalf("canary failed %s", m.Digest)
	}
	if len(m.Scenarios) < 8 {
		t.Fatalf("count %d", len(m.Scenarios))
	}
	m2, _ := Run(now, "3b31ff7879b44c8cb9deb80cdfa37465436964a5")
	if m.Digest != m2.Digest {
		t.Fatal("non-deterministic")
	}
}
