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

	"github.com/jasonhnd/loopcoder/internal/capmatrix"
	"github.com/jasonhnd/loopcoder/internal/supportbundle"
)

func runDiagnose(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("diagnose", flag.ContinueOnError)
	fs.SetOutput(stderr)
	projectID := fs.String("project-id", "", "project id")
	runID := fs.String("run", "", "run id filter")
	maxBytes := fs.Int64("max-bytes", 2<<20, "max archive content bytes")
	dryRun := fs.Bool("dry-run", true, "plan only; no archive (default true)")
	archive := fs.Bool("archive", false, "build local archive (implies not dry-run)")
	output := fs.String("output", "", "local destination directory for archive mode")
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	doDry := *dryRun && !*archive
	if *archive {
		doDry = false
		if strings.TrimSpace(*output) == "" {
			fmt.Fprintln(stderr, "diagnose: --output required for --archive")
			return 2
		}
	}

	now := deps.Now
	if now == nil {
		now = time.Now
	}
	// Real ports: only allowlisted local facts; no invented green path.
	// When no project is given, report partial diagnostics truthfully.
	facts := supportbundle.InputFacts{
		SchemaIntegrity: map[string]string{
			"capmatrix_rows": fmt.Sprintf("%d", len(capmatrix.Matrix())),
			"doctor_codes":   fmt.Sprintf("%d", len(capmatrix.DoctorCodes())),
		},
		TypedDiagnostics: []string{},
	}
	if strings.TrimSpace(*projectID) == "" {
		facts.TypedDiagnostics = append(facts.TypedDiagnostics, "project_id_missing")
	} else {
		facts.TypedDiagnostics = append(facts.TypedDiagnostics, "project_id_present")
		// Process/event facts stay empty unless real stores are wired; do not invent.
		facts.EventTransitions = []string{"status:inspect_only"}
		facts.ProcessTerminalEvidence = []string{"none_observed"}
		facts.CheckNames = []string{"verify", "test", "race", "security"}
	}

	opts := supportbundle.Options{
		ProjectID: *projectID, RunID: *runID, MaxBytes: *maxBytes,
		DryRun: doDry, Dest: *output, BinaryVersion: "0.9.0-dev", Now: now().UTC(),
	}

	if doDry {
		man, err := supportbundle.Plan(opts, facts)
		if err != nil {
			fmt.Fprintf(stderr, "diagnose: plan: %v\n", err)
			return 4
		}
		return emitDiagnose(stdout, *format, map[string]any{
			"mode": "dry_run", "manifest": man, "network_upload": false, "telemetry": supportbundle.TelemetryDefault(),
		})
	}

	bundle, man, err := supportbundle.Build(opts, facts)
	if err != nil {
		fmt.Fprintf(stderr, "diagnose: build: %v\n", err)
		return 4
	}
	if err := os.MkdirAll(*output, 0o700); err != nil {
		fmt.Fprintf(stderr, "diagnose: mkdir: %v\n", err)
		return 4
	}
	path := filepath.Join(*output, "support-bundle.json")
	b, _ := json.MarshalIndent(bundle, "", "  ")
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		fmt.Fprintf(stderr, "diagnose: write: %v\n", err)
		return 4
	}
	mpath := filepath.Join(*output, "manifest.json")
	mb, _ := json.MarshalIndent(man, "", "  ")
	_ = os.WriteFile(mpath, append(mb, '\n'), 0o600)

	return emitDiagnose(stdout, *format, map[string]any{
		"mode": "archive", "manifest": man, "bundle_path": path, "manifest_path": mpath,
		"bundle_digest": supportbundle.Digest(bundle), "network_upload": false, "telemetry": "disabled",
	})
}

func emitDiagnose(stdout io.Writer, format string, payload map[string]any) int {
	if strings.ToLower(format) == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(payload)
		return 0
	}
	fmt.Fprintf(stdout, "mode=%v telemetry=%v network_upload=%v\n", payload["mode"], payload["telemetry"], payload["network_upload"])
	if man, ok := payload["manifest"].(supportbundle.Manifest); ok {
		fmt.Fprintf(stdout, "included=%s\n", strings.Join(man.Included, ","))
		fmt.Fprintf(stdout, "excluded=%s\n", strings.Join(man.Excluded, ","))
		fmt.Fprintf(stdout, "estimated_bytes=%d max_bytes=%d\n", man.EstimatedBytes, man.MaxBytes)
		if len(man.Warnings) > 0 {
			fmt.Fprintf(stdout, "warnings=%s\n", strings.Join(man.Warnings, ";"))
		}
	}
	if p, ok := payload["bundle_path"].(string); ok && p != "" {
		fmt.Fprintf(stdout, "bundle_path=%s\n", p)
	}
	return 0
}

func runCapabilities(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("capabilities", flag.ContinueOnError)
	fs.SetOutput(stderr)
	format := fs.String("format", "text", "text|json")
	area := fs.String("area", "", "optional area filter")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rows := capmatrix.Matrix()
	if a := strings.TrimSpace(*area); a != "" {
		rows = capmatrix.ByArea(a)
	}
	payload := map[string]any{
		"schema":       "loopcoder.capabilities.v1",
		"capabilities": rows,
		"doctor_codes": capmatrix.DoctorCodes(),
		"unsupported":  capmatrix.UnsupportedIDs(),
		"source":       "internal/capmatrix",
	}
	if strings.ToLower(*format) == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(payload)
		return 0
	}
	for _, c := range rows {
		fmt.Fprintf(stdout, "%s\tarea=%s\tsupported=%v\tevidence=%s\t%s\n",
			c.ID, c.Area, c.Supported, c.Evidence, c.Name)
	}
	return 0
}
