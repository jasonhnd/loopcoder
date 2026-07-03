// Package config reads loopcoder repository configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Version      int          `yaml:"version"`
	Adapters     Adapters     `yaml:"adapters"`
	Worker       Worker       `yaml:"worker"`
	Verifier     Verifier     `yaml:"verifier"`
	CI           CI           `yaml:"ci"`
	Verification Verification `yaml:"verification"`
	Resilience   Resilience   `yaml:"resilience"`
	Guardrails   Guardrails   `yaml:"guardrails"`
	Environment  Environment  `yaml:"environment"`
	Evidence     Evidence     `yaml:"evidence"`
	Report       Report       `yaml:"report"`
}

type Adapters struct {
	WorkItems string `yaml:"work_items"`
	Workspace string `yaml:"workspace"`
	Conductor string `yaml:"conductor"`
	Worker    string `yaml:"worker"`
	VCS       string `yaml:"vcs"`
	Verifier  string `yaml:"verifier"`
	Gate      string `yaml:"gate"`
}

// ReviewerNotWorkerWarning returns an advisory warning when reviewer and worker
// roles are configured to the same provider.
func ReviewerNotWorkerWarning(adapters Adapters) string {
	if adapters.Worker == "" || adapters.Verifier == "" || adapters.Worker != adapters.Verifier {
		return ""
	}
	return fmt.Sprintf("adapters.verifier %q matches adapters.worker; reviewer and worker SHOULD differ, but this is advisory only", adapters.Verifier)
}

type Worker struct {
	BaseBranch      string `yaml:"base_branch"`
	Model           string `yaml:"model"`
	ReasoningEffort string `yaml:"reasoning_effort"`
	CommandHint     string `yaml:"command_hint"`
}

type Verifier struct {
	Model           string `yaml:"model"`
	ReasoningEffort string `yaml:"reasoning_effort"`
}

type CI struct {
	Checks    []string `yaml:"checks"`
	Tests     []string `yaml:"tests"`
	Typecheck []string `yaml:"typecheck"`
	Build     []string `yaml:"build"`
}

type Verification struct {
	SpecRequired bool    `yaml:"spec_required"`
	MaxFixPasses int     `yaml:"max_fix_passes"`
	Browser      Browser `yaml:"browser"`
}

type Browser struct {
	Enabled string   `yaml:"enabled"`
	Globs   []string `yaml:"globs"`
}

type Resilience struct {
	Worker   ResilienceWorker   `yaml:"worker"`
	Verifier ResilienceVerifier `yaml:"verifier"`
}

type ResilienceWorker struct {
	HeartbeatIntervalSeconds int   `yaml:"heartbeat_interval_seconds"`
	StaleAfterSeconds        int   `yaml:"stale_after_seconds"`
	HungAfterSeconds         int   `yaml:"hung_after_seconds"`
	MaxAttempts              int   `yaml:"max_attempts"`
	RetryBackoffSeconds      []int `yaml:"retry_backoff_seconds"`
	// Process-watchdog caps for the worker provider CLI (spec 0390, Decision 7).
	// Absent/zero falls back to the built-in default.
	HardCapSeconds      int `yaml:"hard_cap_seconds"`
	StallTimeoutSeconds int `yaml:"stall_timeout_seconds"`
}

// ResilienceVerifier holds process-watchdog caps for the verifier provider CLI.
type ResilienceVerifier struct {
	HardCapSeconds      int `yaml:"hard_cap_seconds"`
	StallTimeoutSeconds int `yaml:"stall_timeout_seconds"`
}

type Report struct {
	Channel string `yaml:"channel"`
}

type Environment struct {
	PreProdBranch string `yaml:"pre_prod_branch"`
}

type Evidence struct {
	Website EvidenceArtifact `yaml:"website"`
	CLI     EvidenceArtifact `yaml:"cli"`
	Library EvidenceArtifact `yaml:"library"`
	App     EvidenceArtifact `yaml:"app"`
}

type EvidenceArtifact struct {
	ProjectType   string `yaml:"-" json:"project_type"`
	PreviewURL    string `yaml:"preview_url" json:"preview_url,omitempty"`
	ExampleOutput string `yaml:"example_output" json:"example_output,omitempty"`
	TestResults   string `yaml:"test_results" json:"test_results,omitempty"`
	PreviewBuild  string `yaml:"preview_build" json:"preview_build,omitempty"`
}

func (e Evidence) Artifacts() []EvidenceArtifact {
	artifacts := make([]EvidenceArtifact, 0, 4)
	for _, candidate := range []EvidenceArtifact{
		e.artifact("website", e.Website),
		e.artifact("cli", e.CLI),
		e.artifact("library", e.Library),
		e.artifact("app", e.App),
	} {
		if !candidate.empty() {
			artifacts = append(artifacts, candidate)
		}
	}
	return artifacts
}

func (e Evidence) artifact(projectType string, artifact EvidenceArtifact) EvidenceArtifact {
	artifact.ProjectType = projectType
	artifact.PreviewURL = strings.TrimSpace(artifact.PreviewURL)
	artifact.ExampleOutput = strings.TrimSpace(artifact.ExampleOutput)
	artifact.TestResults = strings.TrimSpace(artifact.TestResults)
	artifact.PreviewBuild = strings.TrimSpace(artifact.PreviewBuild)
	return artifact
}

func (a EvidenceArtifact) empty() bool {
	return strings.TrimSpace(a.PreviewURL) == "" &&
		strings.TrimSpace(a.ExampleOutput) == "" &&
		strings.TrimSpace(a.TestResults) == "" &&
		strings.TrimSpace(a.PreviewBuild) == ""
}

type Guardrails struct {
	Budget         GuardrailBudget         `yaml:"budget"`
	CircuitBreaker GuardrailCircuitBreaker `yaml:"circuit_breaker"`
}

type GuardrailBudget struct {
	MaxRuns          *int     `yaml:"max_runs"`
	MaxTotalAttempts *int     `yaml:"max_total_attempts"`
	MaxTotalTokens   *int64   `yaml:"max_total_tokens"`
	MaxTotalCostUSD  *float64 `yaml:"max_total_cost_usd"`
}

func (b GuardrailBudget) Enabled() bool {
	return b.MaxRuns != nil ||
		b.MaxTotalAttempts != nil ||
		b.MaxTotalTokens != nil ||
		b.MaxTotalCostUSD != nil
}

type GuardrailCircuitBreaker struct {
	MaxNoProgressWaves    *int `yaml:"max_no_progress_waves"`
	MaxNoProgressAttempts *int `yaml:"max_no_progress_attempts"`
}

func (c GuardrailCircuitBreaker) Enabled() bool {
	return c.MaxNoProgressWaves != nil ||
		c.MaxNoProgressAttempts != nil
}

// Default returns the documented defaults for optional .delivery.yml sections.
func Default() Config {
	return Config{
		Verification: Verification{
			SpecRequired: true,
			MaxFixPasses: 3,
			Browser: Browser{
				Enabled: "auto",
			},
		},
		Resilience: Resilience{
			Worker: ResilienceWorker{
				HeartbeatIntervalSeconds: 15,
				StaleAfterSeconds:        120,
				HungAfterSeconds:         300,
				MaxAttempts:              3,
				RetryBackoffSeconds:      []int{10, 30, 120},
				HardCapSeconds:           1800,
				StallTimeoutSeconds:      120,
			},
			Verifier: ResilienceVerifier{
				HardCapSeconds:      600,
				StallTimeoutSeconds: 120,
			},
		},
		Environment: Environment{
			PreProdBranch: "pre-prod",
		},
	}
}

// Load reads and parses a .delivery.yml file from disk.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read delivery config: %w", err)
	}
	return Parse(data)
}

// Parse reads .delivery.yml data, tolerating absent optional sections.
func Parse(data []byte) (Config, error) {
	cfg := Default()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse delivery config: %w", err)
	}
	if err := validateGuardrailBudget(cfg.Guardrails.Budget); err != nil {
		return Config{}, err
	}
	if err := validateGuardrailCircuitBreaker(cfg.Guardrails.CircuitBreaker); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// ResilienceForRepo loads the resilience config from repoPath/.delivery.yml,
// falling back to built-in defaults when the file is missing or unreadable.
func ResilienceForRepo(repoPath string) Resilience {
	cfg, err := Load(filepath.Join(repoPath, ".delivery.yml"))
	if err != nil {
		return Default().Resilience
	}
	return cfg.Resilience
}

// DurationSeconds converts a positive seconds value to a Duration, falling back
// to def when seconds is zero or negative.
func DurationSeconds(seconds int, def time.Duration) time.Duration {
	if seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return def
}

func validateGuardrailBudget(b GuardrailBudget) error {
	if b.MaxRuns != nil && *b.MaxRuns <= 0 {
		return fmt.Errorf("invalid delivery config: guardrails.budget.max_runs must be greater than zero")
	}
	if b.MaxTotalAttempts != nil && *b.MaxTotalAttempts <= 0 {
		return fmt.Errorf("invalid delivery config: guardrails.budget.max_total_attempts must be greater than zero")
	}
	if b.MaxTotalTokens != nil && *b.MaxTotalTokens <= 0 {
		return fmt.Errorf("invalid delivery config: guardrails.budget.max_total_tokens must be greater than zero")
	}
	if b.MaxTotalCostUSD != nil && *b.MaxTotalCostUSD <= 0 {
		return fmt.Errorf("invalid delivery config: guardrails.budget.max_total_cost_usd must be greater than zero")
	}
	return nil
}

func validateGuardrailCircuitBreaker(c GuardrailCircuitBreaker) error {
	if c.MaxNoProgressWaves != nil && *c.MaxNoProgressWaves <= 0 {
		return fmt.Errorf("invalid delivery config: guardrails.circuit_breaker.max_no_progress_waves must be greater than zero")
	}
	if c.MaxNoProgressAttempts != nil && *c.MaxNoProgressAttempts <= 0 {
		return fmt.Errorf("invalid delivery config: guardrails.circuit_breaker.max_no_progress_attempts must be greater than zero")
	}
	return nil
}
