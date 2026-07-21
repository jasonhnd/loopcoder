package silentcanary

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ManifestSchema is the silent multi-UI canary evidence contract.
const ManifestSchema = "loopcoder.silentcanary.manifest.v1"

// Manifest is a bounded, redacted multi-UI visibility evidence record.
type Manifest struct {
	SchemaVersion     string            `json:"schema_version"`
	ScenarioID        string            `json:"scenario_id"`
	TestedSHA         string            `json:"tested_sha"`
	Variant           string            `json:"variant"`
	SimulatedElapsed  string            `json:"simulated_elapsed"`
	ProviderCalls     int               `json:"provider_calls"`
	ReportKinds       []string          `json:"report_kinds"`
	ClientDigests     map[string]string `json:"client_digests"` // client -> last content digest
	DigestParity      bool              `json:"digest_parity"`
	WorkerRestarts    int               `json:"worker_restarts"`
	SurvivingChildren int               `json:"surviving_children"`
	ReservationHeld   bool              `json:"reservation_held"`
	ResourceState     string            `json:"resource_state"`
	Events            []string          `json:"events"`
	ProcessCleanup    []string          `json:"process_cleanup"`
	Inputs            map[string]string `json:"inputs"`
	Expected          map[string]string `json:"expected"`
	GeneratedAt       time.Time         `json:"generated_at"`
}

var (
	absUserPath = regexp.MustCompile(`/Users/[^/\s"']+`)
	homeAssign  = regexp.MustCompile(`(?i)HOME=`)
	tokenShape  = regexp.MustCompile(`(?i)(ghp_|github_pat_|sk-ant-|sk-|AKIA)[A-Za-z0-9_\-]{8,}`)
	hostID      = regexp.MustCompile(`(?i)(hostname|serial|mac_address|machine.?id)=`)
)

// WriteManifest validates and writes the evidence JSON.
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
	path := filepath.Join(dir, "silentcanary-manifest.json")
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// ValidateNoLeakage fails closed on host paths, secrets, and machine IDs.
func (m Manifest) ValidateNoLeakage() error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	text := string(raw)
	if absUserPath.MatchString(text) {
		return fmt.Errorf("silentcanary: manifest leaks absolute user path")
	}
	if homeAssign.MatchString(text) {
		return fmt.Errorf("silentcanary: manifest leaks HOME assignment")
	}
	if tokenShape.MatchString(text) {
		return fmt.Errorf("silentcanary: manifest leaks secret-shaped token")
	}
	if hostID.MatchString(text) {
		return fmt.Errorf("silentcanary: manifest leaks machine-identifying data")
	}
	if strings.Contains(text, "\"root\"") {
		return fmt.Errorf("silentcanary: manifest must not embed raw root paths")
	}
	return nil
}
