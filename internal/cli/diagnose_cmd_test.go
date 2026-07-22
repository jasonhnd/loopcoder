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

func TestDiagnoseDryRunNoArchive(t *testing.T) {
	deps := DefaultDeps()
	deps.Now = func() time.Time { return time.Date(2026, 7, 22, 23, 0, 0, 0, time.UTC) }
	outDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runDiagnose([]string{
		"--project-id", "proj", "--dry-run", "--format", "json",
	}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["mode"] != "dry_run" {
		t.Fatalf("%+v", payload)
	}
	if payload["network_upload"] != false {
		t.Fatal("network_upload must be false")
	}
	// no files written to outDir
	ents, _ := os.ReadDir(outDir)
	if len(ents) != 0 {
		t.Fatalf("dry-run wrote files: %v", ents)
	}
}

func TestDiagnoseArchiveWritesLocal(t *testing.T) {
	deps := DefaultDeps()
	deps.Now = func() time.Time { return time.Date(2026, 7, 22, 23, 1, 0, 0, time.UTC) }
	outDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runDiagnose([]string{
		"--project-id", "proj", "--archive", "--output", outDir, "--format", "json",
	}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(outDir, "support-bundle.json")); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(outDir, "support-bundle.json"))
	if strings.Contains(string(raw), "/Users/") || strings.Contains(string(raw), "SECRET") {
		t.Fatal("privacy leak in bundle")
	}
}

func TestCapabilitiesJSON(t *testing.T) {
	deps := DefaultDeps()
	var stdout, stderr bytes.Buffer
	code := runCapabilities([]string{"--format", "json"}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["source"] != "internal/capmatrix" {
		t.Fatalf("%+v", payload)
	}
	caps, ok := payload["capabilities"].([]any)
	if !ok || len(caps) == 0 {
		t.Fatalf("empty capabilities")
	}
}

func TestDiagnoseCapabilitiesRegistered(t *testing.T) {
	want := map[string]bool{"diagnose": false, "capabilities": false}
	for _, c := range Commands() {
		if _, ok := want[c.Name]; ok {
			want[c.Name] = true
		}
	}
	for k, v := range want {
		if !v {
			t.Fatalf("missing %s", k)
		}
	}
}
