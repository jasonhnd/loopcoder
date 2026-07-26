package artifactqual

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMeasureLatenciesFromBuiltBinaryCleanHome(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	bin := filepath.Join(t.TempDir(), "loopcoder")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/loopcoder")
	cmd.Dir = root
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build exact binary: %v\n%s", err, raw)
	}
	work := t.TempDir()
	got, err := MeasureLatenciesFromBinary(bin, work, []string{"PATH=" + os.Getenv("PATH")})
	if err != nil {
		t.Fatalf("MeasureLatenciesFromBinary: %v", err)
	}
	if got.RunID != "run_qualify_ui_probe" || got.StartEventID == "" || got.TerminalEventID == "" {
		t.Fatalf("missing exact lifecycle identity: %+v", got)
	}
	if got.StartReportLatencyMs <= 0 || got.RenderedAckLatencyMs <= 0 || got.StatusFreshnessMs <= 0 {
		t.Fatalf("latency metrics must be positive: %+v", got)
	}
	raw, err := os.ReadFile(filepath.Join(work, "latency-home", "projects", "acme-qual-latency", "ui", "reports.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, got.StartEventID) || !strings.Contains(body, got.TerminalEventID) {
		t.Fatal("durable report stream does not bind both observed events")
	}
	cro, err := MeasureCROFromBinary(bin, t.TempDir(), []string{"PATH=" + os.Getenv("PATH")})
	if err != nil {
		t.Fatalf("MeasureCROFromBinary clean home: %v", err)
	}
	if !cro.DecomposeOK || !cro.RoutingOK || !cro.AccountingOK {
		t.Fatalf("structural CRO measurements incomplete: %+v", cro)
	}
	if !cro.ActualUsageEmpty {
		t.Fatal("structural CRO probe must not claim real provider usage")
	}

	cmd = exec.Command(bin, "_qualify-ui-probe",
		"--repo", filepath.Join(work, "latency-repo"),
		"--project-id", "acme-qual-latency",
		"--challenge", strings.Repeat("a", 48))
	cmd.Env = append(os.Environ(),
		"HOME="+t.TempDir(),
		"LOOPCODER_HOME="+t.TempDir(),
		"LOOPCODER_QUALIFY_UI_PROBE_CHALLENGE="+strings.Repeat("b", 48),
	)
	raw, err = cmd.CombinedOutput()
	if err == nil {
		t.Fatal("mismatched internal challenge must fail closed")
	}
	if strings.Contains(string(raw), `"schema":"loopcoder.ui.report.v1"`) {
		t.Fatal("rejected internal probe must not publish UI reports")
	}

	challenge := strings.Repeat("c", 48)
	wrongHome := filepath.Join(t.TempDir(), "latency-home")
	if err := os.Mkdir(wrongHome, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(bin, "_qualify-ui-probe",
		"--repo", filepath.Join(work, "latency-repo"),
		"--project-id", "acme-qual-latency",
		"--challenge", challenge)
	cmd.Env = append(os.Environ(),
		"HOME="+wrongHome,
		"LOOPCODER_HOME="+wrongHome,
		"LOOPCODER_QUALIFY_UI_PROBE_CHALLENGE="+challenge,
	)
	raw, err = cmd.CombinedOutput()
	if err == nil {
		t.Fatal("probe repo outside the bound isolated home parent must fail closed")
	}
	if strings.Contains(string(raw), `"schema":"loopcoder.ui.report.v1"`) {
		t.Fatal("path-rejected internal probe must not publish UI reports")
	}

	cmd = exec.Command(bin, "--help")
	raw, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "_qualify-ui-probe") {
		t.Fatal("internal qualification command must stay hidden from product help")
	}
}

func TestValidateCROBudgetAccountingFailClosed(t *testing.T) {
	valid := `{
		"ok":true,
		"reserved":{"reservation":{"budget_reservation_id":"r1","reserved_value":40,"committed_value":0,"released_value":0,"state":"active"},"replay":false},
		"committed":{"reservation":{"budget_reservation_id":"r1","reserved_value":15,"committed_value":25,"released_value":0,"state":"partially-committed"},"replay":false},
		"released":{"reservation":{"budget_reservation_id":"r1","reserved_value":0,"committed_value":25,"released_value":15,"state":"released"},"replay":false}
	}`
	if !validateCROBudgetAccounting([]byte(valid)) {
		t.Fatal("exact reserve/commit/release arithmetic must pass")
	}
	mutations := []string{
		strings.Replace(valid, `"ok":true`, `"ok":false`, 1),
		strings.Replace(valid, `"budget_reservation_id":"r1","reserved_value":15`, `"budget_reservation_id":"r2","reserved_value":15`, 1),
		strings.Replace(valid, `"committed_value":25,"released_value":15`, `"committed_value":24,"released_value":15`, 1),
		strings.Replace(valid, `"state":"released"},"replay":false`, `"state":"released"},"replay":true`, 1),
		`{"ok":true}`,
	}
	for i, mutated := range mutations {
		if validateCROBudgetAccounting([]byte(mutated)) {
			t.Fatalf("mutation %d must fail closed", i)
		}
	}
}
