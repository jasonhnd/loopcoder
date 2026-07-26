package uiconform_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/uiconform"
)

func TestTerminalFullConformance(t *testing.T) {
	r := &uiconform.Runner{
		ProjectID: "proj_conform",
		Adapter:   "fixture-term-1",
		Version:   "v0.9.0-dev",
		Now:       func() time.Time { return time.Date(2026, 7, 22, 18, 0, 0, 0, time.UTC) },
		Limits:    uiconform.DefaultLimits(),
	}
	m, err := r.RunTerminalFull()
	if err != nil {
		t.Fatal(err)
	}
	if m.Schema != uiconform.SchemaManifest {
		t.Fatalf("schema=%s", m.Schema)
	}
	if m.RealHostClaim {
		t.Fatal("fixture-only must not claim real host")
	}
	if m.Transport != "terminal_jsonl" {
		t.Fatalf("transport=%s", m.Transport)
	}
	if m.Profile != uiconform.ProfileFull && m.Profile != uiconform.ProfileDegraded {
		t.Fatalf("profile=%s", m.Profile)
	}
	// all vectors should pass (including detection vectors)
	for _, v := range m.Vectors {
		if !v.Pass {
			t.Fatalf("vector %s failed: %s", v.Vector, v.Detail)
		}
	}
	// manifest JSON bounded
	b, err := json.Marshal(m)
	if err != nil || len(b) > 64<<10 {
		t.Fatalf("manifest size=%d err=%v", len(b), err)
	}
}

func TestGoldenTranscriptKinds(t *testing.T) {
	doc := uiconform.PublishTranscript("p")
	if doc.Schema != uiconform.SchemaTranscript {
		t.Fatal(doc.Schema)
	}
	if len(doc.Reports) != 6 {
		t.Fatalf("len=%d", len(doc.Reports))
	}
	// unique digests
	seen := map[string]bool{}
	for _, e := range doc.Reports {
		if seen[e.ContentDigest] {
			t.Fatal("duplicate digest in golden")
		}
		seen[e.ContentDigest] = true
	}
}

func TestLyingVectorsDetected(t *testing.T) {
	r := &uiconform.Runner{ProjectID: "p", Adapter: "a", Version: "dev", Now: time.Now, Limits: uiconform.DefaultLimits()}
	m, err := r.RunTerminalFull()
	if err != nil {
		t.Fatal(err)
	}
	want := map[uiconform.VectorID]bool{
		uiconform.VectorLieUnrendered: false,
		uiconform.VectorWrongDigest:   false,
		uiconform.VectorSkipReport:    false,
		uiconform.VectorOutOfOrder:    false,
	}
	for _, v := range m.Vectors {
		if _, ok := want[v.Vector]; ok {
			want[v.Vector] = v.Pass
		}
	}
	for id, ok := range want {
		if !ok {
			t.Fatalf("vector %s not detected", id)
		}
	}
}
