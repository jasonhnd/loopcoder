// Package config reads loopcoder repository configuration.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Version      int          `yaml:"version"`
	Adapters     Adapters     `yaml:"adapters"`
	Worker       Worker       `yaml:"worker"`
	CI           CI           `yaml:"ci"`
	Verification Verification `yaml:"verification"`
	Resilience   Resilience   `yaml:"resilience"`
	Guardrails   Guardrails   `yaml:"guardrails"`
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
	Worker ResilienceWorker `yaml:"worker"`
}

type ResilienceWorker struct {
	HeartbeatIntervalSeconds int   `yaml:"heartbeat_interval_seconds"`
	StaleAfterSeconds        int   `yaml:"stale_after_seconds"`
	HungAfterSeconds         int   `yaml:"hung_after_seconds"`
	MaxAttempts              int   `yaml:"max_attempts"`
	RetryBackoffSeconds      []int `yaml:"retry_backoff_seconds"`
}

type Report struct {
	Channel string `yaml:"channel"`
}

type Guardrails struct {
	Budget GuardrailBudget `yaml:"budget"`
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
			},
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
	return cfg, nil
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
