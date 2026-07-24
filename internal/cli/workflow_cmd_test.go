package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestWorkflowRunOneNodeDevFixture(t *testing.T) {
	t.Setenv("LOOPCODER_HOME", t.TempDir())
	deps := DefaultDeps()
	deps.Now = func() time.Time { return time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC) }
	var stdout, stderr bytes.Buffer
	code := runWorkflow([]string{"run", "--dev-fixture", "one", "--format", "json"}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "DEV_FIXTURE") {
		t.Fatalf("expected DEV_FIXTURE stderr marker, got %q", stderr.String())
	}
	var wrap struct {
		Schema      string `json:"schema"`
		Status      string `json:"status"`
		DevFixture  bool   `json:"dev_fixture"`
		SchemaNote  string `json:"schema_note"`
		ClaimCount  int    `json:"claim_count"`
		LaunchCount int    `json:"launch_count"`
		// Production fields must be absent.
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &wrap); err != nil {
		t.Fatalf("unwrap: %v body=%s", err, stdout.String())
	}
	if wrap.Schema != "loopcoder.workflow.dev_fixture.v1" || wrap.Status != "dev_fixture_complete" || !wrap.DevFixture {
		t.Fatalf("wrap=%+v", wrap)
	}
	if wrap.Result != nil {
		t.Fatalf("dev schema must not embed production result: %s", wrap.Result)
	}
	if strings.Contains(stdout.String(), `"human_gate"`) {
		t.Fatalf("dev fixture JSON must not contain human_gate: %s", stdout.String())
	}
	if wrap.ClaimCount != 1 || wrap.LaunchCount != 1 {
		t.Fatalf("counts=%+v", wrap)
	}
}

func TestWorkflowRunChainDevFixture(t *testing.T) {
	t.Setenv("LOOPCODER_HOME", t.TempDir())
	deps := DefaultDeps()
	deps.Now = func() time.Time { return time.Date(2026, 7, 22, 21, 1, 0, 0, time.UTC) }
	var stdout, stderr bytes.Buffer
	code := runWorkflow([]string{"run", "--dev-fixture", "chain", "--format", "json"}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var wrap struct {
		Status     string   `json:"status"`
		DevFixture bool     `json:"dev_fixture"`
		ClaimCount int      `json:"claim_count"`
		Integrated []string `json:"integrated"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &wrap); err != nil {
		t.Fatalf("unwrap: %v body=%s", err, stdout.String())
	}
	if wrap.Status != "dev_fixture_complete" || !wrap.DevFixture {
		t.Fatalf("wrap=%+v", wrap)
	}
	if wrap.ClaimCount != 3 {
		t.Fatalf("%+v", wrap)
	}
	if strings.Join(wrap.Integrated, ",") != "a,b,c" {
		t.Fatalf("integrated %v", wrap.Integrated)
	}
	if strings.Contains(stdout.String(), `"human_gate"`) {
		t.Fatal("chain dev fixture must not contain human_gate")
	}
}

func TestWorkflowRunRejectsLegacyFixtureAlias(t *testing.T) {
	t.Setenv("LOOPCODER_HOME", t.TempDir())
	deps := DefaultDeps()
	var stdout, stderr bytes.Buffer
	code := runWorkflow([]string{"run", "--fixture", "one", "--format", "json"}, &stdout, &stderr, deps)
	if code == 0 {
		t.Fatal("expected refuse of --fixture")
	}
	if !strings.Contains(stderr.String(), "dev-fixture") {
		t.Fatalf("stderr=%s", stderr.String())
	}
}

func TestWorkflowRunRejectsSilentFixtureDefault(t *testing.T) {
	t.Setenv("LOOPCODER_HOME", t.TempDir())
	deps := DefaultDeps()
	var stdout, stderr bytes.Buffer
	code := runWorkflow([]string{"run", "--format", "json"}, &stdout, &stderr, deps)
	if code == 0 {
		t.Fatalf("expected usage fail without --def/--dev-fixture; stdout=%s", stdout.String())
	}
}

func TestWorkflowRegisteredInCommands(t *testing.T) {
	found := false
	for _, c := range Commands() {
		if c.Name == "workflow" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("workflow command missing")
	}
}
