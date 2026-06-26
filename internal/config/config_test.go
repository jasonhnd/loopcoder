package config

import (
	"reflect"
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
	if cfg.Resilience.Worker.StaleAfterSeconds != 120 {
		t.Fatalf("StaleAfterSeconds = %d, want 120", cfg.Resilience.Worker.StaleAfterSeconds)
	}
	if cfg.Resilience.Worker.HungAfterSeconds != 300 {
		t.Fatalf("HungAfterSeconds = %d, want 300", cfg.Resilience.Worker.HungAfterSeconds)
	}
	if cfg.Resilience.Worker.MaxAttempts != 3 {
		t.Fatalf("MaxAttempts = %d, want 3", cfg.Resilience.Worker.MaxAttempts)
	}
	if !reflect.DeepEqual(cfg.Resilience.Worker.RetryBackoffSeconds, []int{10, 30, 120}) {
		t.Fatalf("RetryBackoffSeconds = %v, want [10 30 120]", cfg.Resilience.Worker.RetryBackoffSeconds)
	}
}

func TestParseReadsConfiguredSections(t *testing.T) {
	data := []byte(`
version: 1
adapters:
  work_items: github
  workspace: git-worktree
  worker: codex
  vcs: github
  verifier: opus
  gate: human-merge
worker:
  base_branch: trunk
  model: gpt-test
  reasoning_effort: high
  command_hint: implement and test
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
`)

	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	if cfg.Adapters.WorkItems != "github" || cfg.Adapters.Workspace != "git-worktree" {
		t.Fatalf("Adapters parsed incorrectly: %#v", cfg.Adapters)
	}
	if cfg.Adapters.Worker != "codex" || cfg.Adapters.VCS != "github" {
		t.Fatalf("Adapters parsed incorrectly: %#v", cfg.Adapters)
	}
	if cfg.Adapters.Verifier != "opus" || cfg.Adapters.Gate != "human-merge" {
		t.Fatalf("Adapters parsed incorrectly: %#v", cfg.Adapters)
	}
	if cfg.Worker.BaseBranch != "trunk" || cfg.Worker.Model != "gpt-test" {
		t.Fatalf("Worker parsed incorrectly: %#v", cfg.Worker)
	}
	if cfg.Worker.ReasoningEffort != "high" || cfg.Worker.CommandHint != "implement and test" {
		t.Fatalf("Worker parsed incorrectly: %#v", cfg.Worker)
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
}
