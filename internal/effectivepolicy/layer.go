package effectivepolicy

import (
	"fmt"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Layer is one configuration contribution before merge.
type Layer struct {
	SchemaVersion        int
	Provider             string
	Model                string
	Effort               string
	Permission           string
	ReportClient         string
	BaseBranch           string
	ProjectPolicyPath    string
	MaxChildProcesses    int
	MaxRSSMiB            int
	RetentionDays        int
	NativeSubagents      *bool
	CompatibilityDerived map[string]bool
}

// Inputs are the ordered configuration surfaces for one resolve.
//
// Environment is accepted only to prove that env-based overrides are ignored
// for pin fields; it never contributes values.
type Inputs struct {
	Explicit      Layer
	RunRequest    Layer
	ProjectPolicy Layer
	UserLocal     Layer
	Defaults      Layer
	// ProjectPolicyYAML, when non-empty, is parsed as project policy.
	ProjectPolicyYAML []byte
	// UserLocalYAML, when non-empty, is parsed as user-local configuration.
	UserLocalYAML []byte
	// Env is optional process environment. Pin keys under LOOPCODER_* must not
	// override explicit pins.
	Env map[string]string
	// Now is the freeze timestamp (time.Time or *time.Time). Digests exclude
	// wall-clock time so identical inputs stay stable across freezes.
	Now any
}

// fileDoc is the on-disk subset for project/user policy files.
type fileDoc struct {
	SchemaVersion     int    `yaml:"schema_version"`
	Provider          string `yaml:"provider"`
	Model             string `yaml:"model"`
	Effort            string `yaml:"effort"`
	Permission        string `yaml:"permission"`
	ReportClient      string `yaml:"report_client"`
	BaseBranch        string `yaml:"base_branch"`
	ProjectPolicyPath string `yaml:"project_policy_path"`
	MaxChildProcesses int    `yaml:"max_child_processes"`
	MaxRSSMiB         int    `yaml:"max_rss_mib"`
	RetentionDays     int    `yaml:"retention_days"`
	NativeSubagents   *bool  `yaml:"native_subagents"`
}

// ParsePolicyFile parses a restricted YAML policy document. Unknown keys fail.
func ParsePolicyFile(data []byte) (Layer, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return Layer{}, fmt.Errorf("effective policy: empty policy document")
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return Layer{}, fmt.Errorf("effective policy: parse yaml: %w", err)
	}
	if err := rejectUnknownKeys(&root); err != nil {
		return Layer{}, err
	}
	var doc fileDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return Layer{}, fmt.Errorf("effective policy: decode yaml: %w", err)
	}
	if doc.SchemaVersion == 0 {
		return Layer{}, fmt.Errorf("effective policy: schema_version is required")
	}
	if doc.SchemaVersion != SchemaVersion {
		return Layer{}, fmt.Errorf("effective policy: incompatible schema_version %d (want %d)", doc.SchemaVersion, SchemaVersion)
	}
	layer := Layer{
		SchemaVersion:     doc.SchemaVersion,
		Provider:          strings.TrimSpace(doc.Provider),
		Model:             strings.TrimSpace(doc.Model),
		Effort:            strings.TrimSpace(doc.Effort),
		Permission:        strings.TrimSpace(doc.Permission),
		ReportClient:      strings.TrimSpace(doc.ReportClient),
		BaseBranch:        strings.TrimSpace(doc.BaseBranch),
		ProjectPolicyPath: strings.TrimSpace(doc.ProjectPolicyPath),
		MaxChildProcesses: doc.MaxChildProcesses,
		MaxRSSMiB:         doc.MaxRSSMiB,
		RetentionDays:     doc.RetentionDays,
		NativeSubagents:   doc.NativeSubagents,
	}
	if err := validateLayerPaths(layer); err != nil {
		return Layer{}, err
	}
	return layer, nil
}

func rejectUnknownKeys(root *yaml.Node) error {
	if root == nil || len(root.Content) == 0 {
		return nil
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return fmt.Errorf("effective policy: document must be a mapping")
	}
	allowed := map[string]struct{}{
		"schema_version": {}, "provider": {}, "model": {}, "effort": {},
		"permission": {}, "report_client": {}, "base_branch": {},
		"project_policy_path": {}, "max_child_processes": {}, "max_rss_mib": {},
		"retention_days": {}, "native_subagents": {},
	}
	for i := 0; i+1 < len(doc.Content); i += 2 {
		key := doc.Content[i].Value
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("effective policy: unknown key %q", key)
		}
	}
	return nil
}

func validateLayerPaths(layer Layer) error {
	path := strings.TrimSpace(layer.ProjectPolicyPath)
	if path == "" {
		return nil
	}
	if filepath.IsAbs(path) {
		return fmt.Errorf("effective policy: project_policy_path must be repository-relative, not absolute")
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return fmt.Errorf("effective policy: project_policy_path escapes repository root")
	}
	if strings.HasPrefix(clean, "/") {
		return fmt.Errorf("effective policy: project_policy_path must be repository-relative")
	}
	return nil
}

// CompiledDefaults returns the safe compiled defaults for v0.9 ordinary runs.
func CompiledDefaults() Layer {
	native := false
	return Layer{
		SchemaVersion:     SchemaVersion,
		BaseBranch:        "pre-prod",
		Permission:        "read_only",
		ReportClient:      "terminal",
		MaxChildProcesses: 8,
		MaxRSSMiB:         2048,
		RetentionDays:     14,
		NativeSubagents:   &native,
	}
}
