package supportbundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capmatrix"
	"github.com/jasonhnd/loopcoder/internal/privacy"
)

// SchemaManifest is the dry-run / archive manifest schema.
const SchemaManifest = "loopcoder.supportbundle.manifest.v1"

// SchemaBundle is the on-disk bundle schema (local only).
const SchemaBundle = "loopcoder.supportbundle.v1"

// Options for diagnose inventory / archive.
type Options struct {
	ProjectID string
	RunID     string
	// Since/Until bound event window (optional).
	Since time.Time
	Until time.Time
	// MaxBytes hard size cap for archive contents.
	MaxBytes int64
	// DryRun produces manifest only.
	DryRun bool
	// Dest is local output directory (required for archive).
	Dest string
	// BinaryVersion for versions section.
	BinaryVersion string
	Now           time.Time
}

// InputFacts are optional local facts already redacted by caller.
type InputFacts struct {
	// EventTransitions are status/ack style transitions (no bodies).
	EventTransitions []string
	// CheckNames from CI.
	CheckNames []string
	// TypedDiagnostics machine codes.
	TypedDiagnostics []string
	// ProcessTerminalEvidence redacted process end states.
	ProcessTerminalEvidence []string
	// SchemaIntegrity summaries (counts/versions only).
	SchemaIntegrity map[string]string
}

// Manifest is owner-reviewable before archive.
type Manifest struct {
	Schema         string            `json:"schema"`
	DryRun         bool              `json:"dry_run"`
	ProjectID      string            `json:"project_id,omitempty"`
	RunID          string            `json:"run_id,omitempty"`
	Included       []string          `json:"included"`
	Excluded       []string          `json:"excluded"`
	EstimatedBytes int64             `json:"estimated_bytes"`
	MaxBytes       int64             `json:"max_bytes"`
	Destination    string            `json:"destination,omitempty"`
	Telemetry      string            `json:"telemetry"` // always "disabled"
	NetworkUpload  bool              `json:"network_upload"`
	PseudonymMap   map[string]string `json:"pseudonym_labels,omitempty"` // labels only, not secrets
	Warnings       []string          `json:"warnings,omitempty"`
}

// Bundle is the local archive payload (never uploaded by this package).
type Bundle struct {
	Schema              string            `json:"schema"`
	CreatedAt           time.Time         `json:"created_at"`
	BinaryVersion       string            `json:"binary_version"`
	CapabilityMatrixIDs []string          `json:"capability_matrix_ids"`
	SchemaIntegrity     map[string]string `json:"schema_integrity,omitempty"`
	EventTransitions    []string          `json:"event_transitions,omitempty"`
	CheckNames          []string          `json:"check_names,omitempty"`
	TypedDiagnostics    []string          `json:"typed_diagnostics,omitempty"`
	ProcessTerminal     []string          `json:"process_terminal_evidence,omitempty"`
	// No source, prompts, tokens, absolute homes, raw logs, provider output.
}

// DefaultExcludes lists content classes never included by default.
func DefaultExcludes() []string {
	return []string{
		"source_code", "issue_pr_body", "prompt", "auth_files", "environment",
		"absolute_home_paths", "raw_logs", "tokens", "provider_responses",
	}
}

// DefaultIncludes lists allowed summary classes.
func DefaultIncludes() []string {
	return []string{
		"versions", "capability_matrix", "schema_integrity_summaries",
		"redacted_event_report_ack_transitions", "process_resource_terminal_evidence",
		"check_names", "typed_diagnostics",
	}
}

// Plan builds a dry-run manifest for owner inspection.
func Plan(opts Options, facts InputFacts) (Manifest, error) {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 2 << 20 // 2 MiB default cap
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	_ = now

	// Estimate size of allowed content only.
	est := int64(4096)
	for _, s := range facts.EventTransitions {
		est += int64(len(s) + 8)
	}
	for _, s := range facts.CheckNames {
		est += int64(len(s) + 4)
	}
	for _, s := range facts.TypedDiagnostics {
		est += int64(len(s) + 4)
	}
	for _, s := range facts.ProcessTerminalEvidence {
		est += int64(len(s) + 8)
	}
	for k, v := range facts.SchemaIntegrity {
		est += int64(len(k) + len(v) + 8)
	}

	m := Manifest{
		Schema: SchemaManifest, DryRun: true,
		ProjectID: opts.ProjectID, RunID: opts.RunID,
		Included:       append([]string(nil), DefaultIncludes()...),
		Excluded:       append([]string(nil), DefaultExcludes()...),
		EstimatedBytes: est, MaxBytes: opts.MaxBytes,
		Destination: path.Base(opts.Dest), // basename only in manifest by default
		Telemetry:   "disabled", NetworkUpload: false,
		PseudonymMap: map[string]string{"project": "proj_*", "run": "run_*"},
	}
	if est > opts.MaxBytes {
		m.Warnings = append(m.Warnings, "estimated content exceeds max_bytes; archive will truncate non-essential sections")
	}
	if !opts.DryRun && strings.TrimSpace(opts.Dest) == "" {
		return m, fmt.Errorf("destination required for archive mode")
	}
	if !opts.DryRun {
		m.DryRun = false
	}
	return m, nil
}

// Build creates a redacted local bundle from facts (no network).
func Build(opts Options, facts InputFacts) (Bundle, Manifest, error) {
	opts.DryRun = false
	man, err := Plan(opts, facts)
	if err != nil {
		return Bundle{}, man, err
	}
	// Scan all string fields for private markers / secrets.
	var scanBlob strings.Builder
	for _, s := range facts.EventTransitions {
		scanBlob.WriteString(s)
		scanBlob.WriteByte('\n')
	}
	for _, s := range facts.ProcessTerminalEvidence {
		scanBlob.WriteString(s)
		scanBlob.WriteByte('\n')
	}
	for _, s := range facts.TypedDiagnostics {
		scanBlob.WriteString(s)
		scanBlob.WriteByte('\n')
	}
	// Redact then scan clean.
	redactedEvents := redactList(facts.EventTransitions)
	redactedProc := redactList(facts.ProcessTerminalEvidence)
	redactedDiag := redactList(facts.TypedDiagnostics)
	clean := strings.Join(append(append(redactedEvents, redactedProc...), redactedDiag...), "\n")
	if findings := privacy.ScanText(privacy.DestCIArtifact, "support_bundle", clean); len(findings) > 0 {
		return Bundle{}, man, fmt.Errorf("privacy scan failed: %w", privacy.AssertClean(findings))
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	ids := make([]string, 0, len(capmatrix.Matrix()))
	for _, c := range capmatrix.Matrix() {
		ids = append(ids, c.ID)
	}
	sort.Strings(ids)

	b := Bundle{
		Schema: SchemaBundle, CreatedAt: now.UTC(),
		BinaryVersion:       opts.BinaryVersion,
		CapabilityMatrixIDs: ids,
		SchemaIntegrity:     copyMap(facts.SchemaIntegrity),
		EventTransitions:    redactedEvents,
		CheckNames:          append([]string(nil), facts.CheckNames...),
		TypedDiagnostics:    redactedDiag,
		ProcessTerminal:     redactedProc,
	}
	// Enforce size cap by dropping process evidence first if needed.
	raw, _ := json.Marshal(b)
	if opts.MaxBytes > 0 && int64(len(raw)) > opts.MaxBytes {
		b.ProcessTerminal = nil
		man.Warnings = append(man.Warnings, "truncated process_terminal_evidence to meet max_bytes")
	}
	man.DryRun = false
	man.EstimatedBytes = int64(len(raw))
	return b, man, nil
}

// TelemetryDefault is always disabled.
func TelemetryDefault() string { return "disabled" }

// Digest of bundle for local integrity.
func Digest(b Bundle) string {
	raw, _ := json.Marshal(b)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func redactList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, privacy.RedactFor(privacy.DestHostDiagnostics, s))
	}
	return out
}

func copyMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
