package supportbundle_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/privacy"
	"github.com/jasonhnd/loopcoder/internal/supportbundle"
)

func fixed() time.Time {
	return time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
}

func TestPlanDryRunNoNetwork(t *testing.T) {
	m, err := supportbundle.Plan(supportbundle.Options{
		ProjectID: "p1", DryRun: true, MaxBytes: 1 << 20, Now: fixed(),
	}, supportbundle.InputFacts{CheckNames: []string{"verify", "test"}})
	if err != nil {
		t.Fatal(err)
	}
	if !m.DryRun || m.NetworkUpload || m.Telemetry != "disabled" {
		t.Fatalf("%#v", m)
	}
	if len(m.Excluded) == 0 || len(m.Included) == 0 {
		t.Fatal("include/exclude")
	}
}

func TestBuildRedactsPrivateMarkers(t *testing.T) {
	facts := supportbundle.InputFacts{
		EventTransitions:        []string{"status=ok " + privacy.MarkerIssue},
		ProcessTerminalEvidence: []string{"exit path=" + privacy.MarkerPath},
		TypedDiagnostics:        []string{"code=ok"},
		CheckNames:              []string{"verify"},
		SchemaIntegrity:         map[string]string{"schema": "v32"},
	}
	b, m, err := supportbundle.Build(supportbundle.Options{
		Dest: "/tmp/out", BinaryVersion: "0.9.0", MaxBytes: 1 << 20, Now: fixed(),
	}, facts)
	if err != nil {
		t.Fatal(err)
	}
	if m.NetworkUpload {
		t.Fatal("upload")
	}
	raw := strings.Join(b.EventTransitions, " ") + strings.Join(b.ProcessTerminal, " ")
	if privacy.ContainsAnyMarker(raw) {
		t.Fatalf("markers remain: %q", raw)
	}
	if supportbundle.TelemetryDefault() != "disabled" {
		t.Fatal()
	}
	if supportbundle.Digest(b) == "" {
		t.Fatal("digest")
	}
	if len(b.CapabilityMatrixIDs) == 0 {
		t.Fatal("matrix ids")
	}
}

func TestArchiveRequiresDest(t *testing.T) {
	_, err := supportbundle.Plan(supportbundle.Options{DryRun: false}, supportbundle.InputFacts{})
	if err == nil {
		t.Fatal("dest required")
	}
}

func TestDefaultExcludesSecrets(t *testing.T) {
	ex := strings.Join(supportbundle.DefaultExcludes(), ",")
	for _, want := range []string{"tokens", "prompt", "raw_logs", "provider_responses"} {
		if !strings.Contains(ex, want) {
			t.Fatalf("missing %s", want)
		}
	}
}
