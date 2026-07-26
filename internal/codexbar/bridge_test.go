package codexbar_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/codexbar"
)

func t0() time.Time { return time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC) }

func TestAbsentNoError(t *testing.T) {
	obs, err := codexbar.Probe(codexbar.ProbeInputs{Present: false, Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if obs.Status != codexbar.StatusAbsent {
		t.Fatalf("%+v", obs)
	}
	if obs.Strategy != "supplement_only" {
		t.Fatal(obs.Strategy)
	}
}

func TestHealthyParse(t *testing.T) {
	raw, _ := json.Marshal(codexbar.RawFixture{
		Version: "1.2.0", Provider: "codex", WindowKind: "five_hour",
		Facts: map[string]string{"remaining": "40", "unit": "percent"},
	})
	obs, err := codexbar.Probe(codexbar.ProbeInputs{Present: true, Output: raw, Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if obs.Status != codexbar.StatusAvailable || obs.Facts["remaining"] != "40" {
		t.Fatalf("%+v", obs)
	}
}

func TestMalformedTimeoutUnsupported(t *testing.T) {
	_, err := codexbar.Probe(codexbar.ProbeInputs{Present: true, Malformed: true, Now: t0})
	if err == nil {
		t.Fatal("expected malformed")
	}
	obs, err := codexbar.Probe(codexbar.ProbeInputs{Present: true, Timeout: true, Now: t0})
	if err == nil || obs.Status != codexbar.StatusTimeout {
		t.Fatalf("%+v err=%v", obs, err)
	}
	obs, _ = codexbar.Probe(codexbar.ProbeInputs{
		Present: true, Version: "0.9.0",
		Output: []byte(`{"version":"0.9.0","provider":"codex"}`), Now: t0,
	})
	if obs.Status != codexbar.StatusUnsupported {
		t.Fatalf("%+v", obs)
	}
}

func TestNoCredentialInFacts(t *testing.T) {
	raw, _ := json.Marshal(codexbar.RawFixture{
		Version: "1.0", Provider: "codex",
		AccountRef: "sk-secret999",
		Facts:      map[string]string{"token": "ghp_xxx", "remaining": "1"},
	})
	obs, err := codexbar.Probe(codexbar.ProbeInputs{Present: true, Output: raw, Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if obs.AccountRef != "redacted" {
		t.Fatal(obs.AccountRef)
	}
	if _, ok := obs.Facts["token"]; ok {
		t.Fatal("token fact leaked")
	}
	if obs.Facts["remaining"] != "1" {
		t.Fatal(obs.Facts)
	}
}

func TestMergeOfficialWinsWhenFresh(t *testing.T) {
	official := map[string]string{"remaining": "10", "limit": "100"}
	bridge := codexbar.Observation{
		Status: codexbar.StatusAvailable,
		Facts:  map[string]string{"remaining": "99", "extra": "x"},
	}
	merged, conf := codexbar.MergeWithOfficial(official, true, bridge)
	if merged["remaining"] != "10" {
		t.Fatalf("override: %v", merged)
	}
	if merged["extra"] != "x" {
		t.Fatal("supplement missing")
	}
	if len(conf) == 0 {
		t.Fatal("expected conflict note")
	}
}

func TestMergeFillsStaleOfficial(t *testing.T) {
	official := map[string]string{"remaining": "10"}
	bridge := codexbar.Observation{
		Status: codexbar.StatusAvailable,
		Facts:  map[string]string{"remaining": "50"},
	}
	merged, conf := codexbar.MergeWithOfficial(official, false, bridge)
	if merged["remaining"] != "50" {
		t.Fatal(merged)
	}
	if len(conf) == 0 {
		t.Fatal("expected fill note")
	}
}

func TestDescriptorAndStep(t *testing.T) {
	d := codexbar.DefaultDescriptor()
	if !d.Optional || d.Authority >= 80 {
		t.Fatalf("%+v", d)
	}
	st := codexbar.AsSourceStep()
	if st.Name != codexbar.SourceID || !st.Optional {
		t.Fatalf("%+v", st)
	}
}

func TestScrubEnv(t *testing.T) {
	out := codexbar.ScrubEnv([]string{"PATH=/bin", "AUTH_TOKEN=x", "FOO=1"})
	for _, e := range out {
		if e == "AUTH_TOKEN=x" {
			t.Fatal(out)
		}
	}
}

func TestRemoveBridgeLeavesOfficial(t *testing.T) {
	// Simulate: official facts alone remain valid when bridge absent.
	official := map[string]string{"remaining": "7"}
	obs, _ := codexbar.Probe(codexbar.ProbeInputs{Present: false, Now: t0})
	merged, conf := codexbar.MergeWithOfficial(official, true, obs)
	if merged["remaining"] != "7" || len(conf) != 0 {
		t.Fatalf("%v %v", merged, conf)
	}
}
