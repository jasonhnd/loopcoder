package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseAppliesDefaultsWhenOptionalSectionsAreAbsent(t *testing.T) {
	cfg, err := Parse([]byte("version: 1\nci:\n  checks: [verify]\n"))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	if cfg.Version != 1 {
		t.Fatalf("Version = %d, want 1", cfg.Version)
	}
	if !cfg.Verification.SpecRequired {
		t.Fatal("Verification.SpecRequired = false, want true")
	}
	if cfg.Verification.MaxFixPasses != 3 {
		t.Fatalf("Verification.MaxFixPasses = %d, want 3", cfg.Verification.MaxFixPasses)
	}
	if cfg.Resilience.Worker.HeartbeatIntervalSeconds != 15 {
		t.Fatalf("HeartbeatIntervalSeconds = %d, want 15", cfg.Resilience.Worker.HeartbeatIntervalSeconds)
	}
	if cfg.Resilience.Worker.StaleAfterSeconds != 300 {
		t.Fatalf("StaleAfterSeconds = %d, want 300", cfg.Resilience.Worker.StaleAfterSeconds)
	}
	if cfg.Resilience.Worker.HungAfterSeconds != 600 {
		t.Fatalf("HungAfterSeconds = %d, want 600", cfg.Resilience.Worker.HungAfterSeconds)
	}
	if cfg.Resilience.Worker.MaxAttempts != 3 {
		t.Fatalf("MaxAttempts = %d, want 3", cfg.Resilience.Worker.MaxAttempts)
	}
	if !reflect.DeepEqual(cfg.Resilience.Worker.RetryBackoffSeconds, []int{10, 30, 120}) {
		t.Fatalf("RetryBackoffSeconds = %v, want [10 30 120]", cfg.Resilience.Worker.RetryBackoffSeconds)
	}
	if cfg.Verifier.Model != "" || cfg.Verifier.ReasoningEffort != "" {
		t.Fatalf("Verifier = %#v, want inherited empty config", cfg.Verifier)
	}
	if cfg.Environment.PreProdBranch != "pre-prod" {
		t.Fatalf("Environment.PreProdBranch = %q, want pre-prod", cfg.Environment.PreProdBranch)
	}
	if got := cfg.Evidence.Artifacts(); len(got) != 0 {
		t.Fatalf("Evidence.Artifacts() = %#v, want empty", got)
	}
}

func TestParseReadsConfiguredSections(t *testing.T) {
	data := []byte(`
version: 1
adapters:
  work_items: github
  workspace: git-worktree
  conductor: opus
  worker: codex
  vcs: github
  verifier: claude
  gate: human-merge
worker:
  base_branch: trunk
  model: gpt-test
  reasoning_effort: high
  command_hint: implement and test
verifier:
  model: claude-test
  reasoning_effort: xhigh
ci:
  checks: [verify, go]
  tests:
    - go test ./...
  typecheck:
    - go vet ./...
  build:
    - go build ./cmd/loopcoder
verification:
  spec_required: false
  max_fix_passes: 5
  browser:
    enabled: never
    globs:
      - web/**
resilience:
  worker:
    heartbeat_interval_seconds: 20
    stale_after_seconds: 140
    hung_after_seconds: 360
    max_attempts: 4
    retry_backoff_seconds: [1, 2, 3]
report:
  channel: chat
environment:
  pre_prod_branch: staging
evidence:
  website:
    preview_url: https://preview.example.com
  cli:
    example_output: |
      $ loopcoder --version
      version=dev
  library:
    test_results: go test ./...
  app:
    preview_build: dist/app-preview.zip
guardrails:
  budget:
    max_runs: 3
    max_total_attempts: 12
    max_total_tokens: 500000
    max_total_cost_usd: 25.50
  circuit_breaker:
    max_no_progress_waves: 2
    max_no_progress_attempts: 3
`)

	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	if cfg.Adapters.WorkItems != "github" || cfg.Adapters.Workspace != "git-worktree" {
		t.Fatalf("Adapters parsed incorrectly: %#v", cfg.Adapters)
	}
	if cfg.Adapters.Conductor != "opus" || cfg.Adapters.Worker != "codex" {
		t.Fatalf("Adapters parsed incorrectly: %#v", cfg.Adapters)
	}
	if cfg.Adapters.VCS != "github" || cfg.Adapters.Verifier != "claude" {
		t.Fatalf("Adapters parsed incorrectly: %#v", cfg.Adapters)
	}
	if cfg.Adapters.Gate != "human-merge" {
		t.Fatalf("Adapters parsed incorrectly: %#v", cfg.Adapters)
	}
	if cfg.Worker.BaseBranch != "trunk" || cfg.Worker.Model != "gpt-test" {
		t.Fatalf("Worker parsed incorrectly: %#v", cfg.Worker)
	}
	if cfg.Worker.ReasoningEffort != "high" || cfg.Worker.CommandHint != "implement and test" {
		t.Fatalf("Worker parsed incorrectly: %#v", cfg.Worker)
	}
	if cfg.Verifier.Model != "claude-test" || cfg.Verifier.ReasoningEffort != "xhigh" {
		t.Fatalf("Verifier parsed incorrectly: %#v", cfg.Verifier)
	}
	if !reflect.DeepEqual(cfg.CI.Checks, []string{"verify", "go"}) {
		t.Fatalf("CI.Checks = %v, want [verify go]", cfg.CI.Checks)
	}
	if !reflect.DeepEqual(cfg.CI.Tests, []string{"go test ./..."}) {
		t.Fatalf("CI.Tests = %v, want [go test ./...]", cfg.CI.Tests)
	}
	if !reflect.DeepEqual(cfg.CI.Typecheck, []string{"go vet ./..."}) {
		t.Fatalf("CI.Typecheck = %v, want [go vet ./...]", cfg.CI.Typecheck)
	}
	if !reflect.DeepEqual(cfg.CI.Build, []string{"go build ./cmd/loopcoder"}) {
		t.Fatalf("CI.Build = %v, want [go build ./cmd/loopcoder]", cfg.CI.Build)
	}
	if cfg.Verification.SpecRequired {
		t.Fatal("Verification.SpecRequired = true, want false")
	}
	if cfg.Verification.MaxFixPasses != 5 {
		t.Fatalf("Verification.MaxFixPasses = %d, want 5", cfg.Verification.MaxFixPasses)
	}
	if cfg.Verification.Browser.Enabled != "never" {
		t.Fatalf("Browser.Enabled = %q, want never", cfg.Verification.Browser.Enabled)
	}
	if !reflect.DeepEqual(cfg.Verification.Browser.Globs, []string{"web/**"}) {
		t.Fatalf("Browser.Globs = %v, want [web/**]", cfg.Verification.Browser.Globs)
	}
	if cfg.Resilience.Worker.HeartbeatIntervalSeconds != 20 {
		t.Fatalf("HeartbeatIntervalSeconds = %d, want 20", cfg.Resilience.Worker.HeartbeatIntervalSeconds)
	}
	if cfg.Resilience.Worker.StaleAfterSeconds != 140 {
		t.Fatalf("StaleAfterSeconds = %d, want 140", cfg.Resilience.Worker.StaleAfterSeconds)
	}
	if cfg.Resilience.Worker.HungAfterSeconds != 360 {
		t.Fatalf("HungAfterSeconds = %d, want 360", cfg.Resilience.Worker.HungAfterSeconds)
	}
	if cfg.Resilience.Worker.MaxAttempts != 4 {
		t.Fatalf("MaxAttempts = %d, want 4", cfg.Resilience.Worker.MaxAttempts)
	}
	if !reflect.DeepEqual(cfg.Resilience.Worker.RetryBackoffSeconds, []int{1, 2, 3}) {
		t.Fatalf("RetryBackoffSeconds = %v, want [1 2 3]", cfg.Resilience.Worker.RetryBackoffSeconds)
	}
	if cfg.Report.Channel != "chat" {
		t.Fatalf("Report.Channel = %q, want chat", cfg.Report.Channel)
	}
	if cfg.Environment.PreProdBranch != "staging" {
		t.Fatalf("Environment.PreProdBranch = %q, want staging", cfg.Environment.PreProdBranch)
	}
	wantEvidence := []EvidenceArtifact{
		{ProjectType: "website", PreviewURL: "https://preview.example.com"},
		{ProjectType: "cli", ExampleOutput: "$ loopcoder --version\nversion=dev"},
		{ProjectType: "library", TestResults: "go test ./..."},
		{ProjectType: "app", PreviewBuild: "dist/app-preview.zip"},
	}
	if !reflect.DeepEqual(cfg.Evidence.Artifacts(), wantEvidence) {
		t.Fatalf("Evidence.Artifacts() = %#v, want %#v", cfg.Evidence.Artifacts(), wantEvidence)
	}
	if cfg.Guardrails.Budget.MaxRuns == nil || *cfg.Guardrails.Budget.MaxRuns != 3 {
		t.Fatalf("Guardrails.Budget.MaxRuns = %#v, want 3", cfg.Guardrails.Budget.MaxRuns)
	}
	if cfg.Guardrails.Budget.MaxTotalAttempts == nil || *cfg.Guardrails.Budget.MaxTotalAttempts != 12 {
		t.Fatalf("Guardrails.Budget.MaxTotalAttempts = %#v, want 12", cfg.Guardrails.Budget.MaxTotalAttempts)
	}
	if cfg.Guardrails.Budget.MaxTotalTokens == nil || *cfg.Guardrails.Budget.MaxTotalTokens != 500000 {
		t.Fatalf("Guardrails.Budget.MaxTotalTokens = %#v, want 500000", cfg.Guardrails.Budget.MaxTotalTokens)
	}
	if cfg.Guardrails.Budget.MaxTotalCostUSD == nil || *cfg.Guardrails.Budget.MaxTotalCostUSD != 25.50 {
		t.Fatalf("Guardrails.Budget.MaxTotalCostUSD = %#v, want 25.50", cfg.Guardrails.Budget.MaxTotalCostUSD)
	}
	if cfg.Guardrails.CircuitBreaker.MaxNoProgressWaves == nil || *cfg.Guardrails.CircuitBreaker.MaxNoProgressWaves != 2 {
		t.Fatalf("Guardrails.CircuitBreaker.MaxNoProgressWaves = %#v, want 2", cfg.Guardrails.CircuitBreaker.MaxNoProgressWaves)
	}
	if cfg.Guardrails.CircuitBreaker.MaxNoProgressAttempts == nil || *cfg.Guardrails.CircuitBreaker.MaxNoProgressAttempts != 3 {
		t.Fatalf("Guardrails.CircuitBreaker.MaxNoProgressAttempts = %#v, want 3", cfg.Guardrails.CircuitBreaker.MaxNoProgressAttempts)
	}
}

func TestParseTreatsEmptyVerifierFieldsAsAbsent(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "quoted empty strings",
			body: `verifier:
  model: ""
  reasoning_effort: ""
`,
		},
		{
			name: "empty scalars",
			body: `verifier:
  model:
  reasoning_effort:
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.body))
			if err != nil {
				t.Fatalf("Parse returned error: %v", err)
			}

			if cfg.Verifier.Model != "" || cfg.Verifier.ReasoningEffort != "" {
				t.Fatalf("Verifier = %#v, want inherited empty config", cfg.Verifier)
			}
		})
	}
}

func TestParseRejectsInvalidGuardrailBudgetCaps(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "max runs zero",
			body: "guardrails:\n  budget:\n    max_runs: 0\n",
			want: "guardrails.budget.max_runs must be greater than zero",
		},
		{
			name: "max attempts negative",
			body: "guardrails:\n  budget:\n    max_total_attempts: -1\n",
			want: "guardrails.budget.max_total_attempts must be greater than zero",
		},
		{
			name: "max tokens zero",
			body: "guardrails:\n  budget:\n    max_total_tokens: 0\n",
			want: "guardrails.budget.max_total_tokens must be greater than zero",
		},
		{
			name: "max cost zero",
			body: "guardrails:\n  budget:\n    max_total_cost_usd: 0\n",
			want: "guardrails.budget.max_total_cost_usd must be greater than zero",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.body))
			if err == nil {
				t.Fatal("Parse returned nil error, want invalid guardrail budget")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestParseRejectsInvalidGuardrailCircuitBreakerCaps(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "max no progress waves zero",
			body: "guardrails:\n  circuit_breaker:\n    max_no_progress_waves: 0\n",
			want: "guardrails.circuit_breaker.max_no_progress_waves must be greater than zero",
		},
		{
			name: "max no progress attempts negative",
			body: "guardrails:\n  circuit_breaker:\n    max_no_progress_attempts: -1\n",
			want: "guardrails.circuit_breaker.max_no_progress_attempts must be greater than zero",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.body))
			if err == nil {
				t.Fatal("Parse returned nil error, want invalid guardrail circuit breaker")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestReviewerNotWorkerWarning(t *testing.T) {
	tests := []struct {
		name     string
		adapters Adapters
		want     string
	}{
		{
			name: "same provider warns",
			adapters: Adapters{
				Worker:   "codex",
				Verifier: "codex",
			},
			want: `adapters.verifier "codex" matches adapters.worker; reviewer and worker SHOULD differ, but this is advisory only`,
		},
		{
			name: "different providers empty",
			adapters: Adapters{
				Worker:   "codex",
				Verifier: "claude",
			},
			want: "",
		},
		{
			name:     "unset providers empty",
			adapters: Adapters{},
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ReviewerNotWorkerWarning(tt.adapters)
			if got != tt.want {
				t.Fatalf("ReviewerNotWorkerWarning() = %q, want %q", got, tt.want)
			}
		})
	}
}
