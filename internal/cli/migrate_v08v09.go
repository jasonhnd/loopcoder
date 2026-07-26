package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/v08export"
	"github.com/jasonhnd/loopcoder/internal/v09import"
)

// runMigrateExportV08 implements loopcoder migrate export-v08 (and export-v08 alias).
func runMigrateExportV08(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("migrate export-v08", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sourceDir := fs.String("source-dir", "", "directory of v0.8 JSON state files (logical sources)")
	exportDir := fs.String("export-dir", "", "export destination outside customer repo (required)")
	customerRepo := fs.String("customer-repo", "", "optional customer repo path; export-dir must not be under it")
	fixture := fs.Bool("fixture", false, "use built-in representative v0.8 fixture sources")
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*exportDir) == "" {
		fmt.Fprintln(stderr, "migrate export-v08: --export-dir is required")
		return 2
	}

	now := deps.Now
	if now == nil {
		now = time.Now
	}

	var files []v08export.SourceFile
	if *fixture {
		files = []v08export.SourceFile{fixtureV08Source()}
	} else if strings.TrimSpace(*sourceDir) != "" {
		var err error
		files, err = loadV08SourceDir(*sourceDir)
		if err != nil {
			fmt.Fprintf(stderr, "migrate export-v08: %v\n", err)
			return 4
		}
	} else {
		fmt.Fprintln(stderr, "migrate export-v08: pass --source-dir or --fixture")
		return 2
	}

	res := v08export.Export(v08export.Input{
		Files: files, ExportDir: *exportDir, CustomerRepoPath: *customerRepo, Now: now().UTC(),
	})
	if !res.Allowed || res.Bundle == nil || res.Manifest == nil {
		fmt.Fprintf(stderr, "migrate export-v08: denied: %s\n", strings.Join(res.Reasons, "; "))
		if strings.ToLower(*format) == "json" {
			_ = json.NewEncoder(stdout).Encode(map[string]any{
				"allowed": false, "reasons": res.Reasons,
			})
		}
		return 4
	}
	if err := v08export.AssertImmutable(res, files); err != nil {
		fmt.Fprintf(stderr, "migrate export-v08: immutability: %v\n", err)
		return 4
	}

	// Persist bundle + manifest under export-dir (outside customer repo).
	if err := os.MkdirAll(*exportDir, 0o700); err != nil {
		fmt.Fprintf(stderr, "migrate export-v08: mkdir: %v\n", err)
		return 4
	}
	bundlePath := filepath.Join(*exportDir, "bundle.json")
	manifestPath := filepath.Join(*exportDir, "manifest.json")
	bb, _ := json.MarshalIndent(res.Bundle, "", "  ")
	mb, _ := json.MarshalIndent(res.Manifest, "", "  ")
	if err := os.WriteFile(bundlePath, append(bb, '\n'), 0o600); err != nil {
		fmt.Fprintf(stderr, "migrate export-v08: write bundle: %v\n", err)
		return 4
	}
	if err := os.WriteFile(manifestPath, append(mb, '\n'), 0o600); err != nil {
		fmt.Fprintf(stderr, "migrate export-v08: write manifest: %v\n", err)
		return 4
	}

	out := map[string]any{
		"allowed":        true,
		"bundle_path":    bundlePath,
		"manifest_path":  manifestPath,
		"bundle_digest":  res.Manifest.BundleDigest,
		"idempotent_key": res.Manifest.IdempotentKey,
		"projects":       len(res.Bundle.Projects),
		"terminal":       len(res.Bundle.TerminalEvidence),
		"unsupported":    len(res.Bundle.Unsupported),
		"warnings":       len(res.Bundle.Warnings),
	}
	if strings.ToLower(*format) == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	} else {
		fmt.Fprintf(stdout, "export_ok=true bundle_digest=%s projects=%d terminal=%d\n",
			res.Manifest.BundleDigest, len(res.Bundle.Projects), len(res.Bundle.TerminalEvidence))
		fmt.Fprintf(stdout, "bundle_path=%s\nmanifest_path=%s\n", bundlePath, manifestPath)
	}
	return 0
}

// runMigrateImportV09 implements loopcoder migrate import-v09 (and import-v09 alias).
func runMigrateImportV09(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("migrate import-v09", flag.ContinueOnError)
	fs.SetOutput(stderr)
	exportDir := fs.String("export-dir", "", "directory containing bundle.json + manifest.json")
	apply := fs.Bool("apply", false, "apply import (default is dry-run with zero writes)")
	dryRun := fs.Bool("dry-run", true, "dry-run validation only (default true; --apply disables)")
	targetHome := fs.String("target-home", "v09-home", "logical target home basename for report paths")
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*exportDir) == "" {
		fmt.Fprintln(stderr, "migrate import-v09: --export-dir is required")
		return 2
	}
	// Default dry-run; --apply means write.
	doDry := *dryRun && !*apply
	if *apply {
		doDry = false
	}

	now := deps.Now
	if now == nil {
		now = time.Now
	}

	bundlePath := filepath.Join(*exportDir, "bundle.json")
	manifestPath := filepath.Join(*exportDir, "manifest.json")
	bb, err := os.ReadFile(bundlePath)
	if err != nil {
		fmt.Fprintf(stderr, "migrate import-v09: read bundle: %v\n", err)
		return 4
	}
	mb, err := os.ReadFile(manifestPath)
	if err != nil {
		fmt.Fprintf(stderr, "migrate import-v09: read manifest: %v\n", err)
		return 4
	}
	var bundle v08export.Bundle
	var man v08export.Manifest
	if err := json.Unmarshal(bb, &bundle); err != nil {
		fmt.Fprintf(stderr, "migrate import-v09: parse bundle: %v\n", err)
		return 4
	}
	if err := json.Unmarshal(mb, &man); err != nil {
		fmt.Fprintf(stderr, "migrate import-v09: parse manifest: %v\n", err)
		return 4
	}

	store := v09import.NewStore()
	res := store.Run(v09import.Input{
		Bundle: &bundle, Manifest: &man,
		ExpectedBundleDigest: man.BundleDigest,
		DryRun:               doDry,
		TargetHome:           *targetHome,
		Now:                  now().UTC(),
	})
	if !res.Allowed {
		fmt.Fprintf(stderr, "migrate import-v09: denied: %s\n", strings.Join(res.Reasons, "; "))
		if strings.ToLower(*format) == "json" {
			_ = json.NewEncoder(stdout).Encode(map[string]any{
				"allowed": false, "reasons": res.Reasons, "dry_run": doDry,
			})
		}
		return 4
	}

	// Second run proves idempotent converge for apply path.
	if !doDry {
		res2 := store.Run(v09import.Input{
			Bundle: &bundle, Manifest: &man,
			ExpectedBundleDigest: man.BundleDigest,
			DryRun:               false,
			TargetHome:           *targetHome,
			Now:                  now().UTC(),
		})
		if !res2.Allowed {
			fmt.Fprintf(stderr, "migrate import-v09: re-import failed: %s\n", strings.Join(res2.Reasons, "; "))
			return 4
		}
		res = res2
	}

	out := map[string]any{
		"allowed":  true,
		"dry_run":  doDry,
		"applied":  !doDry,
		"reasons":  res.Reasons,
		"report":   res.Report,
		"projects": len(store.Projects),
	}
	if strings.ToLower(*format) == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	} else {
		mode := "dry_run"
		if !doDry {
			mode = "applied"
		}
		fmt.Fprintf(stdout, "import_ok=true mode=%s projects=%d\n", mode, len(store.Projects))
		if res.Report != nil {
			fmt.Fprintf(stdout, "bundle_digest=%s target_version=%s\n", res.Report.BundleDigest, res.Report.TargetVersion)
		}
	}
	return 0
}

func fixtureV08Source() v08export.SourceFile {
	body := map[string]any{
		"schema_version": "0.8.1",
		"project": map[string]any{
			"project_id": "p1", "aliases": []string{"app"},
			"repo_owner": "acme", "repo_name": "app",
		},
		"terminal_evidence": []any{
			map[string]any{"kind": "delivery", "id": "d1", "project_id": "p1", "state": "merged"},
		},
	}
	raw, _ := json.Marshal(body)
	return v08export.SourceFile{
		LogicalPath: "global/state.json", Content: raw, Mode: 0o600, SchemaVersion: "0.8.1",
	}
}

func loadV08SourceDir(dir string) ([]v08export.SourceFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []v08export.SourceFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		info, _ := e.Info()
		mode := uint32(0o644)
		if info != nil {
			mode = uint32(info.Mode().Perm())
		}
		ver := "0.8.1"
		var peek map[string]any
		if json.Unmarshal(b, &peek) == nil {
			if s, ok := peek["schema_version"].(string); ok && s != "" {
				ver = s
			}
		}
		files = append(files, v08export.SourceFile{
			LogicalPath: e.Name(), Content: append([]byte(nil), b...), Mode: mode, SchemaVersion: ver,
		})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no .json sources in %s", dir)
	}
	return files, nil
}
