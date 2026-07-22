package v09import_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/v08export"
	"github.com/jasonhnd/loopcoder/internal/v09import"
)

func fixed() time.Time {
	return time.Date(2026, 7, 22, 19, 0, 0, 0, time.UTC)
}

func sampleBundle(t *testing.T) (*v08export.Bundle, string) {
	t.Helper()
	// Build via exporter for realism.
	raw, _ := json.Marshal(map[string]any{
		"schema_version": "0.8.1",
		"project": map[string]any{
			"project_id": "p1", "aliases": []string{"app"},
			"repo_owner": "acme", "repo_name": "app",
		},
		"terminal_evidence": []any{
			map[string]any{"kind": "delivery", "id": "d1", "project_id": "p1", "state": "merged"},
		},
	})
	raw2, _ := json.Marshal(map[string]any{
		"schema_version": "0.8.1",
		"project": map[string]any{
			"project_id": "p2", "aliases": []string{"lib"},
			"repo_owner": "acme", "repo_name": "lib",
		},
		"terminal_evidence": []any{
			map[string]any{"kind": "run", "id": "r1", "project_id": "p2", "state": "delivered"},
		},
	})
	er := v08export.Export(v08export.Input{
		Files: []v08export.SourceFile{
			{LogicalPath: "a.json", Content: raw, Mode: 0o600, SchemaVersion: "0.8.1"},
			{LogicalPath: "b.json", Content: raw2, Mode: 0o600, SchemaVersion: "0.8.1"},
		},
		ExportDir: "/tmp/exports", Now: fixed(),
	})
	if !er.Allowed {
		t.Fatalf("%v", er.Reasons)
	}
	b, _ := json.Marshal(er.Bundle)
	sum := sha256.Sum256(b)
	return er.Bundle, hex.EncodeToString(sum[:])
}

func TestDryRunNoMutation(t *testing.T) {
	bundle, dig := sampleBundle(t)
	s := v09import.NewStore()
	r := s.Run(v09import.Input{
		Bundle: bundle, DryRun: true, ExpectedBundleDigest: dig,
		TargetHome: "home-b", Now: fixed(),
	})
	if !r.Allowed || r.Report == nil {
		t.Fatalf("%v", r.Reasons)
	}
	if !r.Report.DryRun {
		t.Fatal("expected dry-run")
	}
	if s.ProjectCount() != 0 || s.HistoryCount() != 0 {
		t.Fatal("dry-run mutated store")
	}
	if r.Report.Counts["projects"] != 2 {
		t.Fatalf("counts=%v", r.Report.Counts)
	}
	if len(r.Report.TargetPaths) == 0 || r.Report.RequiredSpaceBytes == 0 {
		t.Fatalf("report incomplete: %#v", r.Report)
	}
	if !strings.Contains(r.Report.RollbackLimitation, "backup") {
		t.Fatalf("rollback text: %s", r.Report.RollbackLimitation)
	}
}

func TestImportIdempotent(t *testing.T) {
	bundle, dig := sampleBundle(t)
	s := v09import.NewStore()
	r1 := s.Run(v09import.Input{Bundle: bundle, ExpectedBundleDigest: dig, Now: fixed()})
	if !r1.Allowed {
		t.Fatal(r1.Reasons)
	}
	c1, h1 := s.ProjectCount(), s.HistoryCount()
	r2 := s.Run(v09import.Input{Bundle: bundle, ExpectedBundleDigest: dig, Now: fixed()})
	if !r2.Allowed {
		t.Fatal(r2.Reasons)
	}
	if s.ProjectCount() != c1 || s.HistoryCount() != h1 {
		t.Fatalf("duplicate on reimport: p %d→%d h %d→%d", c1, s.ProjectCount(), h1, s.HistoryCount())
	}
	// skip actions present
	skips := 0
	for _, a := range r2.Report.Actions {
		if strings.HasPrefix(a.Op, "skip_") {
			skips++
		}
	}
	if skips == 0 {
		t.Fatalf("expected skip actions on reimport: %#v", r2.Report.Actions)
	}
}

func TestFailedProjectIsolated(t *testing.T) {
	bundle, dig := sampleBundle(t)
	s := v09import.NewStore()
	r := s.Run(v09import.Input{
		Bundle: bundle, ExpectedBundleDigest: dig,
		FailProjectIDs: []string{"p2"}, Now: fixed(),
	})
	if !r.Allowed {
		t.Fatal(r.Reasons)
	}
	if s.ProjectCount() != 1 {
		t.Fatalf("want only p1 imported, got %d", s.ProjectCount())
	}
	if s.Projects["p1"] == nil {
		t.Fatal("p1 missing after import")
	}
	if len(r.Report.FailedProjects) != 1 || r.Report.FailedProjects[0] != "p2" {
		t.Fatalf("failed=%v", r.Report.FailedProjects)
	}
	if len(r.Report.SucceededProjects) != 1 || r.Report.SucceededProjects[0] != "p1" {
		t.Fatalf("succeeded=%v", r.Report.SucceededProjects)
	}
}

func TestRefuseOverwriteNewer(t *testing.T) {
	bundle, dig := sampleBundle(t)
	s := v09import.NewStore()
	s.MarkNewer("project", "p1")
	r := s.Run(v09import.Input{Bundle: bundle, ExpectedBundleDigest: dig, Now: fixed()})
	if s.Projects["p1"] != nil {
		t.Fatal("must not create overwritten newer project")
	}
	found := false
	for _, c := range r.Report.Conflicts {
		if c.Code == "newer_v09_record" && c.ProjectID == "p1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("conflicts=%#v", r.Report.Conflicts)
	}
	// p2 still imported
	if s.Projects["p2"] == nil {
		t.Fatal("p2 should import")
	}
}

func TestHistoryNeverAuthorizesExecution(t *testing.T) {
	bundle, dig := sampleBundle(t)
	s := v09import.NewStore()
	_ = s.Run(v09import.Input{Bundle: bundle, ExpectedBundleDigest: dig, Now: fixed()})
	for _, h := range s.History {
		if h.AuthorizesExecution {
			t.Fatalf("must not authorize: %#v", h)
		}
		if !h.Historical {
			t.Fatalf("must be historical: %#v", h)
		}
	}
}

func TestDigestMismatchFails(t *testing.T) {
	bundle, _ := sampleBundle(t)
	s := v09import.NewStore()
	r := s.Run(v09import.Input{Bundle: bundle, ExpectedBundleDigest: "deadbeef", Now: fixed()})
	if r.Allowed {
		t.Fatal("digest mismatch must fail")
	}
}

func TestOmissionsFromUnsupported(t *testing.T) {
	bundle, _ := sampleBundle(t)
	bundle.Unsupported = append(bundle.Unsupported, v08export.UnsupportedRecord{Path: "x", Reason: "corrupt"})
	// recompute digest after mutation
	b, _ := json.Marshal(bundle)
	sum := sha256.Sum256(b)
	dig := hex.EncodeToString(sum[:])
	s := v09import.NewStore()
	r := s.Run(v09import.Input{Bundle: bundle, ExpectedBundleDigest: dig, DryRun: true, Now: fixed()})
	if len(r.Report.Omissions) == 0 {
		t.Fatal("expected omissions")
	}
}
