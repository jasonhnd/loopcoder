package directcanary

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ManifestSchema is the direct-path canary evidence contract.
const ManifestSchema = "loopcoder.directcanary.manifest.v1"

// Manifest is a bounded, redacted canary evidence record.
type Manifest struct {
	SchemaVersion      string            `json:"schema_version"`
	ScenarioID         string            `json:"scenario_id"`
	RepoKind           string            `json:"repo_kind"`
	TestedSHA          string            `json:"tested_sha"`
	RequestedRoute     string            `json:"requested_route"`
	ActualRoute        string            `json:"actual_route"`
	RouteMatch         bool              `json:"route_match"`
	WorkerLaunchCount  int               `json:"worker_launch_count"`
	PRNumber           int               `json:"pr_number"`
	HumanGate          string            `json:"human_gate"`
	ProviderCallsCI    int               `json:"provider_calls_during_ci"`
	VerifierAfterReady bool              `json:"verifier_after_ready"`
	Events             []string          `json:"events"`
	SideEffects        []string          `json:"side_effects"`
	ProcessCleanup     []string          `json:"process_cleanup"`
	Residue            []string          `json:"residue"`
	Inputs             map[string]string `json:"inputs"`
	Expected           map[string]string `json:"expected"`
	GeneratedAt        time.Time         `json:"generated_at"`
}

var (
	absUserPath = regexp.MustCompile(`/Users/[^/\s"']+`)
	homeAssign  = regexp.MustCompile(`(?i)HOME=`)
	tokenShape  = regexp.MustCompile(`(?i)(ghp_|github_pat_|sk-ant-|sk-|AKIA)[A-Za-z0-9_\-]{8,}`)
)

// WriteManifest writes JSON under dir after leakage validation.
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
	path := filepath.Join(dir, "directcanary-manifest.json")
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// ValidateNoLeakage fails closed on host paths and secret shapes.
func (m Manifest) ValidateNoLeakage() error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	text := string(raw)
	if absUserPath.MatchString(text) {
		return fmt.Errorf("directcanary: manifest leaks absolute user path")
	}
	if homeAssign.MatchString(text) {
		return fmt.Errorf("directcanary: manifest leaks HOME assignment")
	}
	if tokenShape.MatchString(text) {
		return fmt.Errorf("directcanary: manifest leaks secret-shaped token")
	}
	if strings.Contains(text, "\"root\"") {
		return fmt.Errorf("directcanary: manifest must not embed raw root paths")
	}
	return nil
}
