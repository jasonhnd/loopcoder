package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMigrateExportImportV09RoundTrip(t *testing.T) {
	deps := DefaultDeps()
	deps.Now = func() time.Time { return time.Date(2026, 7, 22, 22, 0, 0, 0, time.UTC) }
	exportDir := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := runMigrate([]string{
		"export-v08", "--fixture", "--export-dir", exportDir, "--format", "json",
	}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("export code=%d stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(exportDir, "bundle.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(exportDir, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	var exp map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &exp); err != nil {
		t.Fatal(err)
	}
	if exp["allowed"] != true {
		t.Fatalf("%+v", exp)
	}

	// dry-run import
	stdout.Reset()
	stderr.Reset()
	code = runMigrate([]string{
		"import-v09", "--export-dir", exportDir, "--format", "json",
	}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("import dry-run code=%d stderr=%s out=%s", code, stderr.String(), stdout.String())
	}
	var imp map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &imp); err != nil {
		t.Fatal(err)
	}
	if imp["dry_run"] != true || imp["allowed"] != true {
		t.Fatalf("%+v", imp)
	}

	// apply
	stdout.Reset()
	stderr.Reset()
	code = runMigrate([]string{
		"import-v09", "--export-dir", exportDir, "--apply", "--format", "json",
	}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("import apply code=%d stderr=%s", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &imp); err != nil {
		t.Fatal(err)
	}
	if imp["applied"] != true {
		t.Fatalf("%+v", imp)
	}
}

func TestExportV08AliasAndHelp(t *testing.T) {
	deps := DefaultDeps()
	deps.Now = func() time.Time { return time.Date(2026, 7, 22, 22, 1, 0, 0, time.UTC) }
	exportDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runMigrateExportV08([]string{"--fixture", "--export-dir", exportDir}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "export_ok=true") {
		t.Fatalf("stdout=%s", stdout.String())
	}

	// help lists subcommands
	stdout.Reset()
	printMigrateHelp(&stdout)
	s := stdout.String()
	for _, want := range []string{"export-v08", "import-v09", "export-v08", "import-v09"} {
		if !strings.Contains(s, want) {
			t.Fatalf("help missing %s: %s", want, s)
		}
	}
}

func TestExportImportCommandsRegistered(t *testing.T) {
	want := map[string]bool{"migrate": false, "export-v08": false, "import-v09": false}
	for _, c := range Commands() {
		if _, ok := want[c.Name]; ok {
			want[c.Name] = true
		}
	}
	for k, v := range want {
		if !v {
			t.Fatalf("missing command %s", k)
		}
	}
}
