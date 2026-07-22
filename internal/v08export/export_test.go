package v08export_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/v08export"
)

func fixed() time.Time {
	return time.Date(2026, 7, 22, 18, 0, 0, 0, time.UTC)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestExportDeterministicSupportedFixture(t *testing.T) {
	body := map[string]any{
		"schema_version": "0.8.1",
		"project": map[string]any{
			"project_id": "p1", "aliases": []string{"app", "acme-app"},
			"repo_owner": "acme", "repo_name": "app",
		},
		"terminal_evidence": []any{
			map[string]any{"kind": "delivery", "id": "d1", "project_id": "p1", "state": "merged"},
		},
		"token": "SECRET_SHOULD_STRIP",
		"lease": map[string]any{"holder": "mac-a"},
		"pid":   12345,
	}
	raw := mustJSON(t, body)
	src := []v08export.SourceFile{{
		LogicalPath: "global/state.json", Content: raw, Mode: 0o600, SchemaVersion: "0.8.1",
	}}
	r := v08export.Export(v08export.Input{
		Files: src, ExportDir: "/tmp/exports/v08", CustomerRepoPath: "/Users/x/repo", Now: fixed(),
	})
	if !r.Allowed {
		t.Fatalf("denied: %v", r.Reasons)
	}
	if err := v08export.AssertImmutable(r, src); err != nil {
		t.Fatal(err)
	}
	// original content still matches
	if string(r.SourceSnapshots["global/state.json"]) != string(raw) {
		t.Fatal("source mutated")
	}
	if len(r.Bundle.Projects) != 1 || r.Bundle.Projects[0].ProjectID != "p1" {
		t.Fatalf("projects=%#v", r.Bundle.Projects)
	}
	if len(r.Bundle.TerminalEvidence) != 1 {
		t.Fatalf("terminal=%#v", r.Bundle.TerminalEvidence)
	}
	// no credential residue in marshaled bundle
	b, _ := json.Marshal(r.Bundle)
	if strings.Contains(string(b), "SECRET_SHOULD_STRIP") {
		t.Fatal("credential leaked into export")
	}
	if strings.Contains(string(b), "12345") {
		t.Fatal("pid authority leaked")
	}
	if r.Manifest.BundleDigest == "" || r.Manifest.IdempotentKey == "" {
		t.Fatalf("manifest incomplete: %#v", r.Manifest)
	}
	// digest of source matches
	sum := sha256.Sum256(raw)
	if r.Bundle.SourceDigests["global/state.json"] != hex.EncodeToString(sum[:]) {
		t.Fatal("source digest mismatch")
	}
}

func TestUnsupportedNewerCorruptPartial(t *testing.T) {
	files := []v08export.SourceFile{
		{LogicalPath: "newer.json", Content: mustJSON(t, map[string]any{"schema_version": "0.9.0", "project": map[string]any{"project_id": "x"}}), Mode: 0o644, SchemaVersion: "0.9.0"},
		{LogicalPath: "corrupt.json", Content: []byte("not-json{"), Mode: 0o644, SchemaVersion: "0.8.0"},
		{LogicalPath: "partial.json", Content: mustJSON(t, map[string]any{"schema_version": "0.8.0"}), Mode: 0o644, SchemaVersion: "0.8.0"},
	}
	// fix corrupt - schema version 0.8.0 but malformed body: peekVersion on not-json returns corrupt
	files[1].SchemaVersion = "" // force peek
	r := v08export.Export(v08export.Input{Files: files, ExportDir: "/tmp/exports", Now: fixed()})
	if !r.Allowed {
		t.Fatalf("%v", r.Reasons)
	}
	if len(r.Bundle.Unsupported) < 2 {
		t.Fatalf("unsupported=%#v", r.Bundle.Unsupported)
	}
	// no auto-migration: zero projects from these
	if len(r.Bundle.Projects) != 0 {
		t.Fatalf("projects=%#v", r.Bundle.Projects)
	}
	if err := v08export.AssertImmutable(r, files); err != nil {
		t.Fatal(err)
	}
}

func TestExportOutsideCustomerRepo(t *testing.T) {
	r := v08export.Export(v08export.Input{
		Files: []v08export.SourceFile{{
			LogicalPath: "s.json", Content: mustJSON(t, map[string]any{"schema_version": "0.8.0"}), Mode: 0o600, SchemaVersion: "0.8.0",
		}},
		ExportDir:        "/Users/x/repo/.loopcoder-export",
		CustomerRepoPath: "/Users/x/repo",
		Now:              fixed(),
	})
	if r.Allowed {
		t.Fatal("export inside customer repo must fail")
	}
}

func TestNonterminalSkipped(t *testing.T) {
	body := map[string]any{
		"schema_version": "0.8.2",
		"project":        map[string]any{"project_id": "p1"},
		"terminal_evidence": []any{
			map[string]any{"kind": "run", "id": "r1", "project_id": "p1", "state": "running"},
			map[string]any{"kind": "run", "id": "r2", "project_id": "p1", "state": "delivered"},
		},
	}
	r := v08export.Export(v08export.Input{
		Files:     []v08export.SourceFile{{LogicalPath: "s.json", Content: mustJSON(t, body), Mode: 0o600, SchemaVersion: "0.8.2"}},
		ExportDir: "/tmp/ex", Now: fixed(),
	})
	if len(r.Bundle.TerminalEvidence) != 1 || r.Bundle.TerminalEvidence[0].ID != "r2" {
		t.Fatalf("%#v", r.Bundle.TerminalEvidence)
	}
}

func TestAliasConflictSurfaced(t *testing.T) {
	f1 := mustJSON(t, map[string]any{
		"schema_version": "0.8.0",
		"project":        map[string]any{"project_id": "p1", "aliases": []string{"app"}},
	})
	f2 := mustJSON(t, map[string]any{
		"schema_version": "0.8.0",
		"project":        map[string]any{"project_id": "p2", "aliases": []string{"app"}},
	})
	r := v08export.Export(v08export.Input{
		Files: []v08export.SourceFile{
			{LogicalPath: "a.json", Content: f1, Mode: 0o600, SchemaVersion: "0.8.0"},
			{LogicalPath: "b.json", Content: f2, Mode: 0o600, SchemaVersion: "0.8.0"},
		},
		ExportDir: "/tmp/ex", Now: fixed(),
	})
	found := false
	for _, w := range r.Bundle.Warnings {
		if w.Code == "alias_conflict" {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings=%#v", r.Bundle.Warnings)
	}
	// both projects present — not silently merged
	if len(r.Bundle.Projects) != 2 {
		t.Fatalf("projects=%#v", r.Bundle.Projects)
	}
}

func TestIdempotentKeyStable(t *testing.T) {
	files := []v08export.SourceFile{{
		LogicalPath: "s.json",
		Content:     mustJSON(t, map[string]any{"schema_version": "0.8.0", "project": map[string]any{"project_id": "p"}}),
		Mode:        0o600, SchemaVersion: "0.8.0",
	}}
	r1 := v08export.Export(v08export.Input{Files: files, ExportDir: "/tmp/ex", Now: fixed()})
	r2 := v08export.Export(v08export.Input{Files: files, ExportDir: "/tmp/ex", Now: fixed()})
	if r1.Manifest.IdempotentKey != r2.Manifest.IdempotentKey {
		t.Fatal("idempotent key drift")
	}
}

func TestNoFilesFails(t *testing.T) {
	r := v08export.Export(v08export.Input{ExportDir: "/tmp/ex", Now: fixed()})
	if r.Allowed {
		t.Fatal("expected fail")
	}
}
