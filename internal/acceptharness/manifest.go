package acceptharness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ManifestSchema is the evidence manifest contract version.
const ManifestSchema = "loopcoder.acceptance_manifest.v1"

// Manifest is a bounded scenario evidence record.
type Manifest struct {
	SchemaVersion  string            `json:"schema_version"`
	ScenarioID     string            `json:"scenario_id"`
	TestedSHA      string            `json:"tested_sha"`
	RepoKind       RepoKind          `json:"repo_kind"`
	Events         []string          `json:"events"`
	SideEffects    []string          `json:"side_effects"`
	ProcessCleanup []string          `json:"process_cleanup"`
	Inputs         map[string]string `json:"inputs"`
	Expected       map[string]string `json:"expected"`
	PolicyDigest   string            `json:"policy_digest,omitempty"`
	// GeneratedAt is scenario clock time, not wall clock when a clock is used.
	GeneratedAt time.Time `json:"generated_at"`
}

var (
	absUserPath = regexp.MustCompile(`/Users/[^/\s]+`)
	homePath    = regexp.MustCompile(`(?i)HOME=`)
	tokenShape  = regexp.MustCompile(`(?i)(ghp_|github_pat_|sk-ant-|sk-|AKIA)[A-Za-z0-9_\-]{8,}`)
)

// WriteManifest writes the manifest as JSON under dir and returns the path.
func WriteManifest(dir string, m Manifest) (string, error) {
	if m.SchemaVersion == "" {
		m.SchemaVersion = ManifestSchema
	}
	if err := m.ValidateNoLeakage(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "acceptance-manifest.json")
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// ValidateNoLeakage fails if the manifest contains host paths or secret shapes.
func (m Manifest) ValidateNoLeakage() error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	text := string(raw)
	if absUserPath.MatchString(text) {
		return fmt.Errorf("manifest leaks absolute user path")
	}
	if homePath.MatchString(text) {
		return fmt.Errorf("manifest leaks HOME assignment")
	}
	if tokenShape.MatchString(text) {
		return fmt.Errorf("manifest leaks secret-shaped token")
	}
	// Repo roots under /var/folders or /tmp are ok if we store only relative
	// synthetic names; ensure we never store the raw Root path fields.
	if strings.Contains(text, "\"root\"") {
		return fmt.Errorf("manifest must not embed raw root paths")
	}
	return nil
}

// HashEvents returns a stable hash of the event list for assertions.
func HashEvents(events []string) string {
	sum := sha256.Sum256([]byte(strings.Join(events, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}
