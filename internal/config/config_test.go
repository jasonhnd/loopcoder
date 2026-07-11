package config

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/migration"
	"gopkg.in/yaml.v3"
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
	if cfg.Verifier.Model != "" || cfg.Verifier.ReasoningEffort != "" {
		t.Fatalf("Verifier = %#v, want inherited empty config", cfg.Verifier)
	}
	if cfg.Models.Strict {
		t.Fatal("Models.Strict = true, want false")
	}
	if cfg.Environment.PreProdBranch != "pre-prod" {
		t.Fatalf("Environment.PreProdBranch = %q, want pre-prod", cfg.Environment.PreProdBranch)
	}
	if got := cfg.Evidence.Artifacts(); len(got) != 0 {
		t.Fatalf("Evidence.Artifacts() = %#v, want empty", got)
	}
}

func TestParseAbsentDomainAndMCPKeepCurrentDefaults(t *testing.T) {
	cfg, err := Parse([]byte("version: 1\nci:\n  checks: [verify]\n"))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	want := Default()
	want.Version = 1
	want.CI.Checks = []string{"verify"}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("Parse without domain/mcp = %#v, want %#v", cfg, want)
	}
}

func TestParseReadsHostProfile(t *testing.T) {
	cfg, err := Parse([]byte("version: 1\nhost:\n  profile: codex\n"))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if cfg.Host.Profile != "codex" {
		t.Fatalf("Host.Profile = %q, want codex", cfg.Host.Profile)
	}
}

func TestParseRejectsUnsafeWorkerAdapterName(t *testing.T) {
	_, err := Parse([]byte("version: 1\nadapters:\n  worker: \"../../evil\"\n"))
	if err == nil {
		t.Fatal("Parse returned nil error, want unsafe worker adapter rejection")
	}
	for _, want := range []string{"adapters.worker", "../../evil", "safe provider adapter name"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want containing %q", err, want)
		}
	}
}

func TestParseRejectsUnsafeVerifierAdapterName(t *testing.T) {
	_, err := Parse([]byte("version: 1\nadapters:\n  verifier: \"bad;provider\"\n"))
	if err == nil {
		t.Fatal("Parse returned nil error, want unsafe verifier adapter rejection")
	}
	for _, want := range []string{"adapters.verifier", "bad;provider", "safe provider adapter name"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want containing %q", err, want)
		}
	}
}

func TestParseRejectsUnknownHostProfile(t *testing.T) {
	_, err := Parse([]byte("version: 1\nhost:\n  profile: unknown-host\n"))
	if err == nil {
		t.Fatal("Parse returned nil error, want invalid host profile")
	}
	for _, want := range []string{"host.profile", "unknown-host", "codex-cli", "claude-code", "paseo-style"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want containing %q", err, want)
		}
	}
}

func TestDefaultMarshalOmitsAbsentDomainAndMCP(t *testing.T) {
	data, err := yaml.Marshal(Default())
	if err != nil {
		t.Fatalf("yaml.Marshal(Default()) returned error: %v", err)
	}
	text := string(data)
	for _, notWant := range []string{"host:", "domain:", "mcp:", "audit:"} {
		if strings.Contains(text, notWant) {
			t.Fatalf("yaml.Marshal(Default()) contained %q:\n%s", notWant, text)
		}
	}
}

func TestParseReadsAuditSection(t *testing.T) {
	data := []byte(`
audit:
  severity_threshold: high
  sast:
    commands:
      - id: govulncheck
        argv: ["govulncheck", "-json", "./..."]
        parser: govulncheck-json
        timeout_seconds: 300
    native:
      secrets: false
      file_permissions: true
      include:
        - "**/*.go"
      exclude:
        - vendor/**
  review:
    rubric_path: docs/security/audit-rubric.md
  baseline:
    path: docs/security/audit-baseline.yml
`)

	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	if cfg.Audit.SeverityThreshold != "high" {
		t.Fatalf("Audit.SeverityThreshold = %q, want high", cfg.Audit.SeverityThreshold)
	}
	if len(cfg.Audit.SAST.Commands) != 1 {
		t.Fatalf("Audit.SAST.Commands len = %d, want 1", len(cfg.Audit.SAST.Commands))
	}
	command := cfg.Audit.SAST.Commands[0]
	if command.ID != "govulncheck" || command.Parser != "govulncheck-json" || command.TimeoutSeconds != 300 {
		t.Fatalf("audit command = %#v, want govulncheck parser/timeout", command)
	}
	if !reflect.DeepEqual(command.Argv, []string{"govulncheck", "-json", "./..."}) {
		t.Fatalf("audit command argv = %#v", command.Argv)
	}
	if cfg.Audit.SAST.Native.Secrets == nil || *cfg.Audit.SAST.Native.Secrets {
		t.Fatalf("native secrets = %#v, want explicit false", cfg.Audit.SAST.Native.Secrets)
	}
	if cfg.Audit.SAST.Native.FilePermissions == nil || !*cfg.Audit.SAST.Native.FilePermissions {
		t.Fatalf("native file_permissions = %#v, want explicit true", cfg.Audit.SAST.Native.FilePermissions)
	}
	if !reflect.DeepEqual(cfg.Audit.SAST.Native.Include, []string{"**/*.go"}) {
		t.Fatalf("native include = %#v", cfg.Audit.SAST.Native.Include)
	}
	if !reflect.DeepEqual(cfg.Audit.SAST.Native.Exclude, []string{"vendor/**"}) {
		t.Fatalf("native exclude = %#v", cfg.Audit.SAST.Native.Exclude)
	}
	if cfg.Audit.Review.RubricPath != "docs/security/audit-rubric.md" {
		t.Fatalf("rubric_path = %q", cfg.Audit.Review.RubricPath)
	}
	if cfg.Audit.Baseline.Path != "docs/security/audit-baseline.yml" {
		t.Fatalf("baseline path = %q", cfg.Audit.Baseline.Path)
	}
}

func TestParseAuditToleratesUnknownFields(t *testing.T) {
	data := []byte(`
audit:
  future: ok
  severity_threshold: medium
  sast:
    future_sast: ok
    commands:
      - id: staticcheck
        argv: ["staticcheck", "-f", "json", "./..."]
        parser: staticcheck-json
        future_command: ok
    native:
      secrets: true
      future_native: ok
  review:
    rubric_path: docs/rubric.md
    future_review: ok
  baseline:
    path: docs/baseline.yml
    future_baseline: ok
`)

	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse returned error for unknown audit fields: %v", err)
	}
	if cfg.Audit.SAST.Commands[0].ID != "staticcheck" || cfg.Audit.Review.RubricPath != "docs/rubric.md" {
		t.Fatalf("audit known fields not parsed: %#v", cfg.Audit)
	}
}

func TestParseRejectsInvalidAuditCommandSpecs(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "bad threshold",
			body: `audit:
  severity_threshold: severe
`,
			want: "audit.severity_threshold",
		},
		{
			name: "empty argv",
			body: `audit:
  sast:
    commands:
      - id: govulncheck
        argv: []
        parser: govulncheck-json
`,
			want: "audit.sast.commands[0].argv",
		},
		{
			name: "missing parser",
			body: `audit:
  sast:
    commands:
      - id: govulncheck
        argv: ["govulncheck", "-json", "./..."]
`,
			want: "audit.sast.commands[0].parser is required",
		},
		{
			name: "negative timeout",
			body: `audit:
  sast:
    commands:
      - id: govulncheck
        argv: ["govulncheck", "-json", "./..."]
        parser: govulncheck-json
        timeout_seconds: -1
`,
			want: "timeout_seconds must not be negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.body))
			if err == nil {
				t.Fatal("Parse returned nil error, want invalid audit config")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestParseReadsDomainAndMCPSections(t *testing.T) {
	data := []byte(`
version: 1
domain:
  name: docs
  description: corporate IR document production
  skills:
    paths:
      - .claude/skills/*/SKILL.md
      - governance/**/skill.md
    machine_library:
      paths:
        - .loopcoder/skill-library/**/*.md
    select:
      - governance
      - disclosure
    prompt_budget_bytes: 4096
  verification:
    rubric:
      paths:
        - governance/qa-checklist.md
      checklist:
        - Rendered PDF matches the approved governance spec.
    review_packet_order:
      - rendered_artifact
      - rubric
      - changed_files
      - diff
      - issue
      - spec
  evidence:
    producer:
      command: make render-ir-pdf
      outputs:
        - out/report.pdf
      timeout_seconds: 300
      include_in_loopreview: true
  red_lines:
    - category: disclosure-compliance
      detail: unresolved disclosure or legal approval requirement
      path_globs:
        - disclosure/**
  partial_work:
    mode: harvest-needs-human
  liveness:
    mode: worktree-mtime
mcp:
  servers:
    - name: governance-index
      transport: stdio
      command: ./tools/governance-mcp
      args: ["--root", "."]
      roles: [worker, verifier]
      read_only: true
    - name: disclosure-system
      transport: http
      url: https://mcp.example.com/disclosure
      auth:
        header: Authorization
        env: DISCLOSURE_MCP_TOKEN
      roles: [worker]
      read_only: false
`)

	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	wantInclude := true
	wantDomain := Domain{
		Name:        "docs",
		Description: "corporate IR document production",
		Skills: DomainSkills{
			Paths: []string{
				".claude/skills/*/SKILL.md",
				"governance/**/skill.md",
			},
			MachineLibrary: DomainMachineLibrary{
				Paths: []string{".loopcoder/skill-library/**/*.md"},
			},
			Select:            []string{"governance", "disclosure"},
			PromptBudgetBytes: 4096,
		},
		Verification: DomainVerification{
			Rubric: DomainRubric{
				Paths:     []string{"governance/qa-checklist.md"},
				Checklist: []string{"Rendered PDF matches the approved governance spec."},
			},
			ReviewPacketOrder: []string{
				"rendered_artifact",
				"rubric",
				"changed_files",
				"diff",
				"issue",
				"spec",
			},
		},
		Evidence: DomainEvidence{
			Producer: DomainEvidenceProducer{
				Command:             "make render-ir-pdf",
				Outputs:             []string{"out/report.pdf"},
				TimeoutSeconds:      300,
				IncludeInLoopreview: &wantInclude,
			},
		},
		RedLines: []DomainRedLine{{
			Category:  "disclosure-compliance",
			Detail:    "unresolved disclosure or legal approval requirement",
			PathGlobs: []string{"disclosure/**"},
		}},
		PartialWork: DomainPartialWork{Mode: "harvest-needs-human"},
		Liveness:    DomainLiveness{Mode: "worktree-mtime"},
	}
	if !reflect.DeepEqual(cfg.Domain, wantDomain) {
		t.Fatalf("Domain = %#v, want %#v", cfg.Domain, wantDomain)
	}

	wantMCP := MCP{
		Servers: []MCPServer{
			{
				Name:      "governance-index",
				Transport: "stdio",
				Command:   "./tools/governance-mcp",
				Args:      []string{"--root", "."},
				Roles:     []string{"worker", "verifier"},
				ReadOnly:  true,
			},
			{
				Name:      "disclosure-system",
				Transport: "http",
				URL:       "https://mcp.example.com/disclosure",
				Auth: MCPAuth{
					Header: "Authorization",
					Env:    "DISCLOSURE_MCP_TOKEN",
				},
				Roles: []string{"worker"},
			},
		},
	}
	if !reflect.DeepEqual(cfg.MCP, wantMCP) {
		t.Fatalf("MCP = %#v, want %#v", cfg.MCP, wantMCP)
	}
}

func TestParseReadsPartialDomainAndMCPSections(t *testing.T) {
	data := []byte(`
domain:
  skills:
    paths:
      - docs/skills/*.md
  evidence:
    producer:
      command: make render
      outputs:
        - out/report.md
mcp:
  servers:
    - name: local-index
      command: ./tools/local-index
`)

	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	if !reflect.DeepEqual(cfg.Domain.Skills.Paths, []string{"docs/skills/*.md"}) {
		t.Fatalf("Domain.Skills.Paths = %#v, want docs/skills/*.md", cfg.Domain.Skills.Paths)
	}
	if cfg.Domain.Evidence.Producer.Command != "make render" {
		t.Fatalf("Domain.Evidence.Producer.Command = %q, want make render", cfg.Domain.Evidence.Producer.Command)
	}
	if !reflect.DeepEqual(cfg.Domain.Evidence.Producer.Outputs, []string{"out/report.md"}) {
		t.Fatalf("Domain.Evidence.Producer.Outputs = %#v, want out/report.md", cfg.Domain.Evidence.Producer.Outputs)
	}
	if cfg.Domain.Evidence.Producer.IncludeInLoopreview != nil {
		t.Fatalf("IncludeInLoopreview = %#v, want nil for absent optional bool", cfg.Domain.Evidence.Producer.IncludeInLoopreview)
	}
	if !reflect.DeepEqual(cfg.Domain.Verification, DomainVerification{}) {
		t.Fatalf("Domain.Verification = %#v, want zero value", cfg.Domain.Verification)
	}
	if len(cfg.MCP.Servers) != 1 || cfg.MCP.Servers[0].Name != "local-index" {
		t.Fatalf("MCP.Servers = %#v, want one partial local-index server", cfg.MCP.Servers)
	}
	if cfg.MCP.Servers[0].Auth != (MCPAuth{}) || cfg.MCP.Servers[0].ReadOnly {
		t.Fatalf("partial MCP server = %#v, want zero auth and read_only false", cfg.MCP.Servers[0])
	}
}

func TestParseReadsDomainCommandArgvFields(t *testing.T) {
	cfg, err := Parse([]byte(`
domain:
  evidence:
    producer:
      argv: ["make", "render-ir-pdf"]
      outputs:
        - out/report.pdf
  liveness:
    mode: custom
    argv: ["./tools/liveness", "--worktree", "."]
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if !reflect.DeepEqual(cfg.Domain.Evidence.Producer.Argv, []string{"make", "render-ir-pdf"}) {
		t.Fatalf("Producer.Argv = %#v, want make render-ir-pdf", cfg.Domain.Evidence.Producer.Argv)
	}
	if !reflect.DeepEqual(cfg.Domain.Liveness.Argv, []string{"./tools/liveness", "--worktree", "."}) {
		t.Fatalf("Liveness.Argv = %#v, want no-shell liveness argv", cfg.Domain.Liveness.Argv)
	}
}

func TestParseRejectsInvalidDomainCommandSpecs(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "producer command and argv",
			body: `
domain:
  evidence:
    producer:
      command: make render
      argv: ["make", "render"]
      outputs: [out/report.md]
`,
			want: "domain.evidence.producer.command and domain.evidence.producer.argv are mutually exclusive",
		},
		{
			name: "producer empty argv",
			body: `
domain:
  evidence:
    producer:
      argv: []
      outputs: [out/report.md]
`,
			want: "domain.evidence.producer.argv must be a non-empty array",
		},
		{
			name: "producer empty argv element",
			body: `
domain:
  evidence:
    producer:
      argv: ["make", ""]
      outputs: [out/report.md]
`,
			want: "domain.evidence.producer.argv[1] must not be empty",
		},
		{
			name: "producer missing outputs",
			body: `
domain:
  evidence:
    producer:
      argv: ["make", "render"]
`,
			want: "domain.evidence.producer.outputs is required",
		},
		{
			name: "liveness custom missing command",
			body: `
domain:
  liveness:
    mode: custom
`,
			want: "domain.liveness.mode custom requires exactly one",
		},
		{
			name: "liveness command and argv",
			body: `
domain:
  liveness:
    mode: custom
    command: echo alive
    argv: ["echo", "alive"]
`,
			want: "domain.liveness.command and domain.liveness.argv are mutually exclusive",
		},
		{
			name: "liveness command without custom mode",
			body: `
domain:
  liveness:
    mode: log-only
    command: echo alive
`,
			want: "domain.liveness.command and domain.liveness.argv are only valid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.body))
			if err == nil {
				t.Fatal("Parse returned nil error, want invalid domain command spec")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestParseDomainAndMCPTolerateUnknownFields(t *testing.T) {
	data := []byte(`
version: 1
future_top_level: ok
domain:
  future_domain_field: ok
  skills:
    future_skills_field: ok
    paths:
      - governance/skill.md
  evidence:
    producer:
      command: make render
      outputs:
        - out/report.md
      future_producer_field: ok
mcp:
  future_mcp_field: ok
  servers:
    - name: governance-index
      transport: http
      url: https://mcp.example.com/governance
      future_server_field: ok
      auth:
        header: Authorization
        env: GOVERNANCE_MCP_TOKEN
        future_auth_field: ok
`)

	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse returned error for unknown fields: %v", err)
	}
	if !reflect.DeepEqual(cfg.Domain.Skills.Paths, []string{"governance/skill.md"}) {
		t.Fatalf("Domain.Skills.Paths = %#v, want governance/skill.md", cfg.Domain.Skills.Paths)
	}
	if cfg.Domain.Evidence.Producer.Command != "make render" {
		t.Fatalf("Domain.Evidence.Producer.Command = %q, want make render", cfg.Domain.Evidence.Producer.Command)
	}
	if len(cfg.MCP.Servers) != 1 || cfg.MCP.Servers[0].Name != "governance-index" || cfg.MCP.Servers[0].Auth.Header != "Authorization" {
		t.Fatalf("MCP.Servers = %#v, want known fields parsed despite unknown fields", cfg.MCP.Servers)
	}
}

func TestParseRejectsUnknownMCPRole(t *testing.T) {
	_, err := Parse([]byte(`
mcp:
  servers:
    - name: future
      roles: [planner]
`))
	if err == nil {
		t.Fatal("Parse returned nil error for unknown MCP role")
	}
	if !strings.Contains(err.Error(), "mcp.servers[0].roles") || !strings.Contains(err.Error(), "planner") {
		t.Fatalf("Parse error = %v, want MCP role context", err)
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
models:
  strict: true
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
	if !cfg.Models.Strict {
		t.Fatalf("Models parsed incorrectly: %#v", cfg.Models)
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

func TestParseReadsAndValidatesProviderInventoryProfileOverrides(t *testing.T) {
	cfg, err := Parse([]byte(`
version: 1
provider_inventory:
  executables:
    claude: ["/opt/claude"]
  profile_overrides:
    claude: acct_abcdefghijklmnopqrstuvwxyz234567
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if got := cfg.ProviderInventory.ProfileOverrides["claude"]; got != "acct_abcdefghijklmnopqrstuvwxyz234567" {
		t.Fatalf("profile override = %q", got)
	}

	_, err = Parse([]byte(`
version: 1
provider_inventory:
  profile_overrides:
    claude: "alice@example.com"
`))
	if err == nil {
		t.Fatal("Parse returned nil error for display-like profile override")
	}
	if !strings.Contains(err.Error(), "account_profile_id") {
		t.Fatalf("Parse error = %v, want account_profile_id context", err)
	}
}

func TestParseReadsLegacyReportConfig(t *testing.T) {
	data := []byte("version: 1\n" + migration.LegacyReportConfigRoot + ":\n  channel: legacy-chat\n")

	cfg, diagnostics, err := ParseWithDiagnostics(data)
	if err != nil {
		t.Fatalf("ParseWithDiagnostics returned error: %v", err)
	}

	if cfg.Report.Channel != "legacy-chat" {
		t.Fatalf("Report.Channel = %q, want legacy-chat", cfg.Report.Channel)
	}
	assertMigrationDiagnostic(t, diagnostics, migration.LegacyReportConfigRoot, false)
	assertMigrationDiagnostic(t, diagnostics, migration.LegacyReportConfigChannel, false)
}

func TestParsePrefersReportConfigOverLegacyConflict(t *testing.T) {
	data := []byte("version: 1\nreport:\n  channel: current-chat\n" + migration.LegacyReportConfigRoot + ":\n  channel: legacy-chat\n")

	cfg, diagnostics, err := ParseWithDiagnostics(data)
	if err != nil {
		t.Fatalf("ParseWithDiagnostics returned error: %v", err)
	}

	if cfg.Report.Channel != "current-chat" {
		t.Fatalf("Report.Channel = %q, want current-chat", cfg.Report.Channel)
	}
	assertMigrationDiagnostic(t, diagnostics, migration.LegacyReportConfigRoot, true)
	assertMigrationDiagnostic(t, diagnostics, migration.LegacyReportConfigChannel, true)
}

func TestMigrateDeliveryYAMLRewritesLegacyReportConfig(t *testing.T) {
	data := []byte("version: 1\n" + migration.LegacyReportConfigRoot + ":\n  channel: legacy-chat\nci:\n  checks: [verify]\n")

	result, err := MigrateDeliveryYAML(data)
	if err != nil {
		t.Fatalf("MigrateDeliveryYAML returned error: %v", err)
	}
	if !result.Changed {
		t.Fatal("MigrateDeliveryYAML changed = false, want true")
	}
	text := string(result.Data)
	if strings.Contains(text, migration.LegacyReportConfigRoot+":") {
		t.Fatalf("migrated config still contains legacy root key:\n%s", text)
	}
	for _, want := range []string{"report:", "channel: legacy-chat", "ci:", "checks: [verify]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("migrated config missing %q:\n%s", want, text)
		}
	}

	cfg, err := Parse(result.Data)
	if err != nil {
		t.Fatalf("Parse migrated config returned error: %v", err)
	}
	if cfg.Report.Channel != "legacy-chat" {
		t.Fatalf("Report.Channel = %q, want legacy-chat", cfg.Report.Channel)
	}
}

func TestMigrateDeliveryYAMLRemovesLegacyWhenReportExists(t *testing.T) {
	data := []byte("version: 1\nreport:\n  channel: current-chat\n" + migration.LegacyReportConfigRoot + ":\n  channel: legacy-chat\n")

	result, err := MigrateDeliveryYAML(data)
	if err != nil {
		t.Fatalf("MigrateDeliveryYAML returned error: %v", err)
	}
	if !result.Changed {
		t.Fatal("MigrateDeliveryYAML changed = false, want true")
	}
	text := string(result.Data)
	if strings.Contains(text, migration.LegacyReportConfigRoot+":") {
		t.Fatalf("migrated config still contains legacy root key:\n%s", text)
	}
	if !strings.Contains(text, "channel: current-chat") {
		t.Fatalf("migrated config lost current report value:\n%s", text)
	}
	assertMigrationDiagnostic(t, result.Diagnostics, migration.LegacyReportConfigRoot, true)
}

func TestLoadForRepoWarnsForLegacyReportConfig(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte("version: 1\n"+migration.LegacyReportConfigRoot+":\n  channel: legacy-chat\n"), 0o644); err != nil {
		t.Fatalf("write .delivery.yml: %v", err)
	}
	var warnings strings.Builder

	cfg, err := LoadForRepo(context.Background(), repo, LoadOptions{Warnings: &warnings})
	if err != nil {
		t.Fatalf("LoadForRepo returned error: %v", err)
	}
	if cfg.Report.Channel != "legacy-chat" {
		t.Fatalf("Report.Channel = %q, want legacy-chat", cfg.Report.Channel)
	}
	for _, want := range []string{migration.LegacyReportConfigRoot, migration.ReportConfigRoot, "loopcoder doctor --repo . --fix"} {
		if !strings.Contains(warnings.String(), want) {
			t.Fatalf("warning missing %q:\n%s", want, warnings.String())
		}
	}
}

func assertMigrationDiagnostic(t *testing.T, diagnostics []migration.Diagnostic, legacy string, conflict bool) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if diagnostic.Legacy != legacy {
			continue
		}
		if diagnostic.Conflict != conflict {
			t.Fatalf("diagnostic for %q conflict = %v, want %v", legacy, diagnostic.Conflict, conflict)
		}
		if diagnostic.Current == "" || diagnostic.FixCommand == "" {
			t.Fatalf("diagnostic for %q missing current/fix fields: %#v", legacy, diagnostic)
		}
		return
	}
	t.Fatalf("diagnostic for %q missing in %#v", legacy, diagnostics)
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
