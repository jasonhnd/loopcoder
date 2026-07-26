package v08export

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SupportedSchemaVersions are the v0.8 schema versions this exporter accepts.
var SupportedSchemaVersions = []string{"0.8.0", "0.8.1", "0.8.2"}

// SchemaExport is the neutral export document schema.
const SchemaExport = "loopcoder.v08export.bundle.v1"

// SchemaManifest is the export manifest schema.
const SchemaManifest = "loopcoder.v08export.manifest.v1"

// SourceFile is one read-only source artifact observed for export.
type SourceFile struct {
	// LogicalPath is a fixture path key (not written back).
	LogicalPath string
	// Content is the raw bytes; hash is computed over this.
	Content []byte
	// Mode is the file mode bits for immutability assertions.
	Mode uint32
	// SchemaVersion declared inside the content (if any).
	SchemaVersion string
}

// V08Project is a normalized project identity from v0.8.
type V08Project struct {
	ProjectID string   `json:"project_id"`
	Aliases   []string `json:"aliases,omitempty"`
	RepoOwner string   `json:"repo_owner,omitempty"`
	RepoName  string   `json:"repo_name,omitempty"`
}

// V08TerminalEvidence is selected terminal work/run/delivery/report evidence.
type V08TerminalEvidence struct {
	Kind      string `json:"kind"` // work|run|delivery|report
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	State     string `json:"state"` // must be terminal
	// Digest of source payload (not raw unsafe content).
	PayloadDigest string `json:"payload_digest"`
}

// Warning is a non-fatal classification note.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// UnsupportedRecord is data that cannot be exported safely.
type UnsupportedRecord struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Bundle is the versioned neutral export.
type Bundle struct {
	Schema           string                `json:"schema"`
	ExportedAt       time.Time             `json:"exported_at"`
	SourceVersions   []string              `json:"source_versions"`
	Projects         []V08Project          `json:"projects"`
	TerminalEvidence []V08TerminalEvidence `json:"terminal_evidence"`
	Unsupported      []UnsupportedRecord   `json:"unsupported,omitempty"`
	Warnings         []Warning             `json:"warnings,omitempty"`
	// SourceDigests map logical path → sha256 of original bytes.
	SourceDigests map[string]string `json:"source_digests"`
	// Counts for V090-070 importer.
	Counts map[string]int `json:"counts"`
}

// Manifest accompanies the bundle with immutability evidence.
type Manifest struct {
	Schema        string            `json:"schema"`
	BundleDigest  string            `json:"bundle_digest"`
	SourceDigests map[string]string `json:"source_digests"`
	SourceModes   map[string]uint32 `json:"source_modes"`
	RecordCounts  map[string]int    `json:"record_counts"`
	Warnings      int               `json:"warnings"`
	Unsupported   int               `json:"unsupported"`
	IdempotentKey string            `json:"idempotent_key"`
	// ExportLocation is outside customer repo (logical).
	ExportLocation string `json:"export_location"`
}

// Result is one export attempt.
type Result struct {
	Allowed  bool
	Reasons  []string
	Bundle   *Bundle
	Manifest *Manifest
	// SourceSnapshots for tests to assert byte-for-byte identity.
	SourceSnapshots map[string][]byte
	SourceModes     map[string]uint32
}

// Input configures a read-only export from in-memory fixtures (CI-safe).
type Input struct {
	// Files are the immutable sources to read.
	Files []SourceFile
	// ExportDir is a location outside the customer repo (logical path).
	ExportDir string
	// Now injects time.
	Now time.Time
	// CustomerRepoPath if set must not be a prefix of ExportDir.
	CustomerRepoPath string
}

// Export reads sources immutably and builds a neutral bundle + manifest.
func Export(in Input) Result {
	res := Result{
		SourceSnapshots: map[string][]byte{},
		SourceModes:     map[string]uint32{},
	}
	if strings.TrimSpace(in.ExportDir) == "" {
		res.Reasons = append(res.Reasons, "export directory required (outside customer repo)")
		return res
	}
	if in.CustomerRepoPath != "" && strings.HasPrefix(in.ExportDir, in.CustomerRepoPath) {
		res.Reasons = append(res.Reasons, "export must be produced outside customer repo")
		return res
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	supported := map[string]bool{}
	for _, v := range SupportedSchemaVersions {
		supported[v] = true
	}

	bundle := &Bundle{
		Schema:         SchemaExport,
		ExportedAt:     now.UTC(),
		SourceDigests:  map[string]string{},
		Counts:         map[string]int{},
		SourceVersions: nil,
	}
	versionSet := map[string]bool{}
	projectsByID := map[string]*V08Project{}
	// conflict detection for aliases
	aliasOwner := map[string]string{}

	for _, f := range in.Files {
		// Snapshot original bytes and mode — never mutate.
		orig := append([]byte(nil), f.Content...)
		res.SourceSnapshots[f.LogicalPath] = orig
		res.SourceModes[f.LogicalPath] = f.Mode
		sum := sha256.Sum256(orig)
		digest := hex.EncodeToString(sum[:])
		bundle.SourceDigests[f.LogicalPath] = digest

		ver := strings.TrimSpace(f.SchemaVersion)
		if ver == "" {
			// try parse from content
			ver = peekVersion(orig)
		}
		if ver == "" {
			bundle.Unsupported = append(bundle.Unsupported, UnsupportedRecord{
				Path: f.LogicalPath, Reason: "missing_schema_version",
			})
			continue
		}
		if !supported[ver] {
			// newer/unknown — fail closed for that record, no auto-migration
			if isNewer(ver) {
				bundle.Unsupported = append(bundle.Unsupported, UnsupportedRecord{
					Path: f.LogicalPath, Reason: "newer_unsupported_schema:" + ver,
				})
			} else {
				bundle.Unsupported = append(bundle.Unsupported, UnsupportedRecord{
					Path: f.LogicalPath, Reason: "unknown_or_corrupt_schema:" + ver,
				})
			}
			bundle.Warnings = append(bundle.Warnings, Warning{
				Code: "unsupported_schema", Message: f.LogicalPath + " schema " + ver,
			})
			continue
		}
		versionSet[ver] = true

		rec, err := parseRecord(orig)
		if err != nil {
			bundle.Unsupported = append(bundle.Unsupported, UnsupportedRecord{
				Path: f.LogicalPath, Reason: "malformed:" + err.Error(),
			})
			continue
		}

		// Strip credentials / leases / PIDs — never import.
		if rec.HasCredentials || rec.HasLiveLease || rec.HasPIDAuthority {
			bundle.Warnings = append(bundle.Warnings, Warning{
				Code: "stripped_unsafe", Message: f.LogicalPath + " dropped credential/lease/pid fields",
			})
		}

		if rec.Project != nil {
			id := rec.Project.ProjectID
			if id == "" {
				bundle.Unsupported = append(bundle.Unsupported, UnsupportedRecord{
					Path: f.LogicalPath, Reason: "project_missing_id",
				})
			} else if existing, ok := projectsByID[id]; ok {
				// reconcile by evidence; surface alias conflicts
				for _, a := range rec.Project.Aliases {
					if prev, taken := aliasOwner[a]; taken && prev != id {
						bundle.Warnings = append(bundle.Warnings, Warning{
							Code: "alias_conflict", Message: a + " claimed by " + prev + " and " + id,
						})
						continue
					}
					aliasOwner[a] = id
					if !contains(existing.Aliases, a) {
						existing.Aliases = append(existing.Aliases, a)
					}
				}
			} else {
				p := *rec.Project
				// redacted: drop any private-looking fields already stripped in parse
				projectsByID[id] = &p
				for _, a := range p.Aliases {
					if prev, taken := aliasOwner[a]; taken && prev != id {
						bundle.Warnings = append(bundle.Warnings, Warning{
							Code: "alias_conflict", Message: a + " claimed by " + prev + " and " + id,
						})
					} else {
						aliasOwner[a] = id
					}
				}
			}
		}

		for _, te := range rec.Terminal {
			// Only terminal states.
			if !isTerminal(te.State) {
				bundle.Warnings = append(bundle.Warnings, Warning{
					Code: "nonterminal_skipped", Message: te.ID + " state " + te.State,
				})
				continue
			}
			// Payload digest only — no raw unsafe payload.
			if te.PayloadDigest == "" && rec.RawPayload != nil {
				s := sha256.Sum256(rec.RawPayload)
				te.PayloadDigest = hex.EncodeToString(s[:])
			}
			// Ensure no credential material in digest path — raw already hashed.
			bundle.TerminalEvidence = append(bundle.TerminalEvidence, te)
		}
	}

	for v := range versionSet {
		bundle.SourceVersions = append(bundle.SourceVersions, v)
	}
	sort.Strings(bundle.SourceVersions)

	for _, p := range projectsByID {
		sort.Strings(p.Aliases)
		bundle.Projects = append(bundle.Projects, *p)
	}
	sort.Slice(bundle.Projects, func(i, j int) bool {
		return bundle.Projects[i].ProjectID < bundle.Projects[j].ProjectID
	})
	sort.Slice(bundle.TerminalEvidence, func(i, j int) bool {
		return bundle.TerminalEvidence[i].ID < bundle.TerminalEvidence[j].ID
	})

	bundle.Counts["projects"] = len(bundle.Projects)
	bundle.Counts["terminal_evidence"] = len(bundle.TerminalEvidence)
	bundle.Counts["unsupported"] = len(bundle.Unsupported)
	bundle.Counts["warnings"] = len(bundle.Warnings)
	bundle.Counts["sources"] = len(in.Files)

	// If everything unsupported and no projects — still allowed as empty export
	// with manifest, unless no files at all.
	if len(in.Files) == 0 {
		res.Reasons = append(res.Reasons, "no source files")
		return res
	}

	raw, _ := json.Marshal(bundle)
	bsum := sha256.Sum256(raw)
	// Idempotent key from sorted source digests.
	var digests []string
	for p, d := range bundle.SourceDigests {
		digests = append(digests, p+"="+d)
	}
	sort.Strings(digests)
	ikey := sha256.Sum256([]byte(strings.Join(digests, "|")))

	modes := map[string]uint32{}
	for p, m := range res.SourceModes {
		modes[p] = m
	}

	manifest := &Manifest{
		Schema:         SchemaManifest,
		BundleDigest:   hex.EncodeToString(bsum[:]),
		SourceDigests:  copyMap(bundle.SourceDigests),
		SourceModes:    modes,
		RecordCounts:   copyIntMap(bundle.Counts),
		Warnings:       len(bundle.Warnings),
		Unsupported:    len(bundle.Unsupported),
		IdempotentKey:  hex.EncodeToString(ikey[:8]),
		ExportLocation: in.ExportDir,
	}

	res.Allowed = true
	res.Bundle = bundle
	res.Manifest = manifest
	res.Reasons = append(res.Reasons, fmt.Sprintf(
		"exported projects=%d terminal=%d unsupported=%d",
		len(bundle.Projects), len(bundle.TerminalEvidence), len(bundle.Unsupported),
	))
	return res
}

// AssertImmutable verifies source snapshots still match original content/modes.
func AssertImmutable(res Result, original []SourceFile) error {
	for _, f := range original {
		got, ok := res.SourceSnapshots[f.LogicalPath]
		if !ok {
			return fmt.Errorf("missing snapshot %s", f.LogicalPath)
		}
		if string(got) != string(f.Content) {
			return fmt.Errorf("content mutated for %s", f.LogicalPath)
		}
		if res.SourceModes[f.LogicalPath] != f.Mode {
			return fmt.Errorf("mode mutated for %s", f.LogicalPath)
		}
	}
	return nil
}

// --- internal parse helpers ---

type parsed struct {
	Project         *V08Project
	Terminal        []V08TerminalEvidence
	HasCredentials  bool
	HasLiveLease    bool
	HasPIDAuthority bool
	RawPayload      []byte
}

func parseRecord(raw []byte) (*parsed, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	p := &parsed{}
	// Detect unsafe fields — strip, never export values.
	for k := range m {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "token") || strings.Contains(lk, "password") ||
			strings.Contains(lk, "credential") || strings.Contains(lk, "api_key") ||
			strings.Contains(lk, "apikey") || strings.Contains(lk, "secret") {
			p.HasCredentials = true
			delete(m, k)
		}
		if strings.Contains(lk, "lease") {
			p.HasLiveLease = true
			delete(m, k)
		}
		if lk == "pid" || strings.Contains(lk, "pid_authority") || strings.HasSuffix(lk, "_pid") {
			p.HasPIDAuthority = true
			delete(m, k)
		}
	}
	if proj, ok := m["project"].(map[string]any); ok {
		pp := &V08Project{
			ProjectID: str(proj["project_id"]),
			RepoOwner: str(proj["repo_owner"]),
			RepoName:  str(proj["repo_name"]),
		}
		if al, ok := proj["aliases"].([]any); ok {
			for _, a := range al {
				if s, ok := a.(string); ok && s != "" {
					pp.Aliases = append(pp.Aliases, s)
				}
			}
		}
		p.Project = pp
	}
	if arr, ok := m["terminal_evidence"].([]any); ok {
		for _, el := range arr {
			em, ok := el.(map[string]any)
			if !ok {
				continue
			}
			p.Terminal = append(p.Terminal, V08TerminalEvidence{
				Kind:          str(em["kind"]),
				ID:            str(em["id"]),
				ProjectID:     str(em["project_id"]),
				State:         str(em["state"]),
				PayloadDigest: str(em["payload_digest"]),
			})
		}
	}
	if rawPay, ok := m["payload"]; ok {
		b, _ := json.Marshal(rawPay)
		p.RawPayload = b
	}
	return p, nil
}

func peekVersion(raw []byte) string {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "corrupt"
	}
	if v, ok := m["schema_version"].(string); ok {
		return v
	}
	if v, ok := m["version"].(string); ok {
		return v
	}
	return ""
}

func isNewer(ver string) bool {
	// crude: 0.9.x or 0.8.3+ considered newer than supported set
	if strings.HasPrefix(ver, "0.9") || strings.HasPrefix(ver, "1.") {
		return true
	}
	if strings.HasPrefix(ver, "0.8.") {
		// 0.8.3+
		for _, s := range SupportedSchemaVersions {
			if s == ver {
				return false
			}
		}
		return true
	}
	return false
}

func isTerminal(state string) bool {
	switch strings.ToLower(state) {
	case "merged", "closed", "delivered", "gated", "terminal", "done", "success", "failed":
		return true
	default:
		return false
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func contains(ss []string, x string) bool {
	for _, s := range ss {
		if s == x {
			return true
		}
	}
	return false
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyIntMap(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
